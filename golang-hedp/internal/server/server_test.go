package server_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sashaakr/research/golang-hedp/internal/catalog"
	"github.com/sashaakr/research/golang-hedp/internal/library"
	"github.com/sashaakr/research/golang-hedp/internal/render"
	"github.com/sashaakr/research/golang-hedp/internal/server"
)

func libraryRoot(tb testing.TB) string {
	tb.Helper()
	dir, err := os.Getwd()
	if err != nil {
		tb.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		candidate := filepath.Join(dir, "testdata", "library")
		if _, err := os.Stat(filepath.Join(candidate, "blueprint.yaml")); err == nil {
			return candidate
		}
		dir = filepath.Dir(dir)
	}
	tb.Skip("chart library not generated; run: go run ./cmd/chartgen")
	return ""
}

func newTestServer(tb testing.TB) http.Handler {
	tb.Helper()
	h, _ := newTestServerWithRegistry(tb)
	return h
}

func newTestServerWithRegistry(tb testing.TB) (http.Handler, *library.Registry) {
	tb.Helper()
	root := libraryRoot(tb)

	reg := library.NewRegistry(library.Config{
		RenderOptions: render.Options{ReleaseConcurrency: 1, CachedEngine: true},
	})
	if _, err := reg.LoadDirectory("rev-one", root); err != nil {
		tb.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return server.NewServer(logger, reg, server.Config{MaxConfigs: 100}), reg
}

func post(tb testing.TB, h http.Handler, path string, body any) *httptest.ResponseRecorder {
	tb.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		tb.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestLibraryEndpointReportsTheByteSplit(t *testing.T) {
	h := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/library", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	var resp struct {
		Catalog  catalog.Stats `json:"catalog"`
		Releases []string      `json:"releases"`
		Features []string      `json:"features"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Catalog.TemplateBytes == 0 || resp.Catalog.CRDBytes == 0 {
		t.Errorf("expected a non-trivial byte split, got %+v", resp.Catalog)
	}
	if len(resp.Releases) == 0 || len(resp.Features) == 0 {
		t.Error("expected releases and features to be reported")
	}
}

func TestPlanIsCheapAndExplainsPulledInReleases(t *testing.T) {
	h := newTestServer(t)

	rec := post(t, h, "/v1/plan", map[string]any{
		"id": "plan-test", "tier": "standard", "region": "eu-west-1",
		"features": []string{"observability"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", rec.Code, rec.Body)
	}

	var resp struct {
		Selected []string   `json:"selected"`
		PulledIn []string   `json:"pulled_in"`
		Waves    [][]string `json:"waves"`
		Count    int        `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Count == 0 || len(resp.Waves) < 2 {
		t.Fatalf("expected a multi-wave plan, got count=%d waves=%d", resp.Count, len(resp.Waves))
	}
	// A feature-gated release that depends on a base release must drag that
	// base release in even though the customer never asked for it.
	if len(resp.PulledIn) == 0 {
		t.Error("expected at least one release pulled in by a dependency")
	}
}

func TestRenderRejectsInvalidConfigWith422(t *testing.T) {
	h := newTestServer(t)

	rec := post(t, h, "/v1/render", map[string]any{
		"id": "NOPE", "tier": "gold", "region": "mars",
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("got %d, want 422: %s", rec.Code, rec.Body)
	}
	var resp struct {
		Revision string `json:"revision"`
		render.Result
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Revision != "rev-one" {
		t.Errorf("response revision is %q, want rev-one", resp.Revision)
	}
	if len(resp.Problems) < 3 {
		t.Errorf("expected problems for id, tier and region, got %v", resp.Problems)
	}
	if len(resp.Releases) != 0 {
		t.Error("nothing should have been rendered")
	}
}

func TestBulkStreamsResultsThenSummary(t *testing.T) {
	h := newTestServer(t)

	configs := []map[string]any{
		{"id": "bulk-a", "tier": "free", "region": "eu-west-1"},
		{"id": "bulk-b", "tier": "premium", "region": "us-east-1", "features": []string{"observability"}},
		{"id": "BAD-ID", "tier": "free", "region": "eu-west-1"},
	}
	rec := post(t, h, "/v1/render/bulk", map[string]any{"configs": configs, "concurrency": 2})

	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/x-ndjson" {
		t.Errorf("content type is %q, want application/x-ndjson", ct)
	}

	var results, summaries int
	var invalid int
	scanner := bufio.NewScanner(rec.Body)
	scanner.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for scanner.Scan() {
		var env struct {
			Type    string              `json:"type"`
			Result  *render.Result      `json:"result"`
			Summary *render.BulkSummary `json:"summary"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &env); err != nil {
			t.Fatalf("bad NDJSON line %q: %v", scanner.Text(), err)
		}
		switch env.Type {
		case "result":
			results++
			if len(env.Result.Problems) > 0 {
				invalid++
			}
		case "summary":
			summaries++
			if env.Summary.Configs != 3 {
				t.Errorf("summary counted %d configs, want 3", env.Summary.Configs)
			}
			if env.Summary.Invalid != 1 {
				t.Errorf("summary counted %d invalid, want 1", env.Summary.Invalid)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}

	if results != 3 {
		t.Errorf("got %d results, want 3", results)
	}
	if summaries != 1 {
		t.Errorf("got %d summary lines, want 1", summaries)
	}
	// One bad config must not sink the batch - that is the whole point of
	// reporting per-config verdicts instead of failing the request.
	if invalid != 1 {
		t.Errorf("got %d invalid results, want 1", invalid)
	}
}

func TestBulkRejectsOversizedBatch(t *testing.T) {
	h := newTestServer(t)

	configs := make([]map[string]any, 101)
	for i := range configs {
		configs[i] = map[string]any{"id": "x", "tier": "free", "region": "eu-west-1"}
	}
	rec := post(t, h, "/v1/render/bulk", map[string]any{"configs": configs})
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %d, want 413: %s", rec.Code, rec.Body)
	}
}

func TestUnknownFieldIsRejected(t *testing.T) {
	h := newTestServer(t)

	// A typo in a field name should be a 400, not a silently ignored setting
	// that makes a customer's render quietly wrong.
	rec := post(t, h, "/v1/render", map[string]any{
		"id": "typo-test", "tier": "free", "region": "eu-west-1", "featurez": []string{"observability"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400: %s", rec.Code, rec.Body)
	}
}

// --- versioned libraries -----------------------------------------------------

func TestUnknownRevisionIs404NotASilentFallback(t *testing.T) {
	h := newTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/render?revision=deadbeef",
		bytes.NewReader([]byte(`{"id":"acme","tier":"free","region":"eu-west-1"}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// The entire point of pinning a revision is that the wrong charts are worse
	// than no charts. Falling back to the default would render a different
	// commit and report success.
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404: %s", rec.Code, rec.Body)
	}
}

func TestPushVersionThenRenderAgainstIt(t *testing.T) {
	h, reg := newTestServerWithRegistry(t)

	data, err := library.PackFast(libraryRoot(t))
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPut, "/v1/versions/commit-abc", bytes.NewReader(data))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("push returned %d: %s", rec.Code, rec.Body)
	}

	var pushed struct {
		Revision string        `json:"revision"`
		Loaded   bool          `json:"loaded"`
		Stats    library.Stats `json:"stats"`
		Default  string        `json:"default"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &pushed); err != nil {
		t.Fatal(err)
	}
	if !pushed.Loaded || pushed.Stats.Charts == 0 {
		t.Fatalf("push reported nothing loaded: %+v", pushed)
	}
	// Pushing must not silently move the default.
	if pushed.Default != "rev-one" {
		t.Errorf("default moved to %q on push; it should stay rev-one", pushed.Default)
	}

	// The pushed revision renders.
	req = httptest.NewRequest(http.MethodPost, "/v1/render?revision=commit-abc",
		bytes.NewReader([]byte(`{"id":"acme","tier":"premium","region":"eu-west-1","features":["observability"]}`)))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("render against pushed revision returned %d: %s", rec.Code, rec.Body)
	}

	var res struct {
		Revision string `json:"revision"`
		render.Result
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Revision != "commit-abc" || !res.OK || res.Manifests == 0 {
		t.Fatalf("unexpected result: revision=%s ok=%v manifests=%d", res.Revision, res.OK, res.Manifests)
	}

	// Both revisions are resident and listed.
	if _, _, count := reg.Usage(); count != 2 {
		t.Errorf("%d versions resident, want 2", count)
	}
}

func TestVersionLifecycleEndpoints(t *testing.T) {
	h, _ := newTestServerWithRegistry(t)

	data, err := library.PackFast(libraryRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPut, "/v1/versions/v2", bytes.NewReader(data))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("push returned %d: %s", rec.Code, rec.Body)
	}

	// Promote it.
	req = httptest.NewRequest(http.MethodPost, "/v1/versions/v2/default", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("set-default returned %d: %s", rec.Code, rec.Body)
	}

	// An unqualified render now uses it.
	rec = post(t, h, "/v1/render", map[string]any{"id": "acme", "tier": "free", "region": "eu-west-1"})
	var res struct {
		Revision string `json:"revision"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Revision != "v2" {
		t.Errorf("unqualified render used %q, want v2", res.Revision)
	}

	// List reports both, with accounting.
	req = httptest.NewRequest(http.MethodGet, "/v1/versions", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var list struct {
		Default   string          `json:"default"`
		Versions  []library.Stats `json:"versions"`
		UsedBytes int64           `json:"used_bytes"`
		Budget    int64           `json:"budget_bytes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if list.Default != "v2" || len(list.Versions) != 2 {
		t.Errorf("unexpected listing: default=%s versions=%d", list.Default, len(list.Versions))
	}
	if list.UsedBytes == 0 || list.UsedBytes > list.Budget {
		t.Errorf("accounting looks wrong: used=%d budget=%d", list.UsedBytes, list.Budget)
	}

	// Evict the old one.
	req = httptest.NewRequest(http.MethodDelete, "/v1/versions/rev-one", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete returned %d: %s", rec.Code, rec.Body)
	}
	req = httptest.NewRequest(http.MethodDelete, "/v1/versions/rev-one", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("deleting an absent revision returned %d, want 404", rec.Code)
	}
}

func TestPushRejectsGarbageBundle(t *testing.T) {
	h := newTestServer(t)

	req := httptest.NewRequest(http.MethodPut, "/v1/versions/bad", bytes.NewReader([]byte("not a tar")))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("got %d, want 422: %s", rec.Code, rec.Body)
	}
}

// --- running behind a load balancer ------------------------------------------

// stubFetcher stands in for the artifact store a pod pulls bundles from.
type stubFetcher struct {
	data    []byte
	calls   atomic.Int64
	missing map[string]bool
	delay   time.Duration
}

func (f *stubFetcher) Fetch(ctx context.Context, revision string) ([]byte, error) {
	f.calls.Add(1)
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.missing[revision] {
		return nil, fmt.Errorf("%w: no bundle for %s", library.ErrRevisionNotFound, revision)
	}
	return f.data, nil
}

// TestReplicaPullsUnknownRevision is the behaviour horizontal scaling depends
// on. A version pushed to one pod is invisible to its siblings, so a pod asked
// for a revision it does not hold must fetch it rather than 404.
func TestReplicaPullsUnknownRevision(t *testing.T) {
	data, err := library.PackFast(libraryRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	fetcher := &stubFetcher{data: data, missing: map[string]bool{"never-built": true}}

	reg := library.NewRegistry(library.Config{
		RenderOptions: render.Options{ReleaseConcurrency: 1},
		Fetcher:       fetcher,
	})
	if _, err := reg.LoadDirectory("rev-one", libraryRoot(t)); err != nil {
		t.Fatal(err)
	}
	h := server.NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)), reg, server.Config{MaxConfigs: 100})

	// A revision this pod has never seen is pulled and served.
	req := httptest.NewRequest(http.MethodPost, "/v1/render?revision=pushed-elsewhere",
		bytes.NewReader([]byte(`{"id":"acme","tier":"free","region":"eu-west-1"}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", rec.Code, rec.Body)
	}
	if fetcher.calls.Load() != 1 {
		t.Errorf("fetcher called %d times, want 1", fetcher.calls.Load())
	}

	// It is now resident, so a second request must not re-fetch.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/render?revision=pushed-elsewhere",
		bytes.NewReader([]byte(`{"id":"acme","tier":"free","region":"eu-west-1"}`)))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || fetcher.calls.Load() != 1 {
		t.Errorf("second request re-fetched: code=%d calls=%d", rec.Code, fetcher.calls.Load())
	}

	// A revision that genuinely does not exist stays a 404 - still never a
	// silent fallback to the default.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/render?revision=never-built",
		bytes.NewReader([]byte(`{"id":"acme","tier":"free","region":"eu-west-1"}`)))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("absent revision got %d, want 404: %s", rec.Code, rec.Body)
	}
}

// TestConcurrentMissesFetchOnce guards the thundering herd. Every replica sees
// the burst for a newly built commit at once; at ~134 MB a copy, fetching it
// once per in-flight request is how a pod runs out of memory.
func TestConcurrentMissesFetchOnce(t *testing.T) {
	data, err := library.PackFast(libraryRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	fetcher := &stubFetcher{data: data, delay: 50 * time.Millisecond}

	reg := library.NewRegistry(library.Config{
		RenderOptions: render.Options{ReleaseConcurrency: 1},
		Fetcher:       fetcher,
	})

	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := reg.GetOrFetch(context.Background(), "hot-commit"); err != nil {
				t.Errorf("fetch failed: %v", err)
			}
		}()
	}
	wg.Wait()

	if n := fetcher.calls.Load(); n != 1 {
		t.Errorf("fetched %d times for one cold revision, want 1", n)
	}
	if _, _, count := reg.Usage(); count != 1 {
		t.Errorf("%d versions resident, want 1", count)
	}
}

// TestShedsLoadAtCapacity checks the admission limit. A saturated pod gains no
// throughput from extra concurrency, so it must refuse and let the load
// balancer try a replica that can actually help.
func TestShedsLoadAtCapacity(t *testing.T) {
	reg := library.NewRegistry(library.Config{
		RenderOptions: render.Options{ReleaseConcurrency: 1, CachedEngine: true},
	})
	if _, err := reg.LoadDirectory("rev-one", libraryRoot(t)); err != nil {
		t.Fatal(err)
	}
	h := server.NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)), reg, server.Config{
		MaxConfigs:  100,
		MaxInFlight: 1,
	})

	// Hold the single slot with a render that takes a while.
	release := make(chan struct{})
	started := make(chan struct{})
	go func() {
		configs := make([]map[string]any, 40)
		for i := range configs {
			configs[i] = map[string]any{
				"id": fmt.Sprintf("hold-%03d", i), "tier": "enterprise",
				"region": "eu-west-1", "render_all": true,
			}
		}
		body, _ := json.Marshal(map[string]any{"configs": configs, "concurrency": 1})
		req := httptest.NewRequest(http.MethodPost, "/v1/render/bulk", bytes.NewReader(body))
		close(started)
		h.ServeHTTP(httptest.NewRecorder(), req)
		close(release)
	}()

	<-started
	// Give the in-flight counter a moment to register before probing.
	deadline := time.Now().Add(5 * time.Second)
	var shed *httptest.ResponseRecorder
	for time.Now().Before(deadline) {
		rec := post(t, h, "/v1/render", map[string]any{"id": "probe", "tier": "free", "region": "eu-west-1"})
		if rec.Code == http.StatusServiceUnavailable {
			shed = rec
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if shed == nil {
		t.Fatal("never observed a shed request while the pod was at capacity")
	}
	if ra := shed.Header().Get("Retry-After"); ra == "" {
		t.Error("shed response carries no Retry-After, so a client cannot back off sensibly")
	}

	// Probes must keep answering at capacity, or Kubernetes pulls a busy pod
	// out of rotation exactly when it is doing useful work.
	for _, path := range []string{"/healthz", "/readyz", "/v1/versions"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s returned %d while at capacity, want 200", path, rec.Code)
		}
	}
	<-release
}

func TestReadyzGatesOnResidentVersion(t *testing.T) {
	empty := library.NewRegistry(library.Config{})
	h := server.NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)), empty, server.Config{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("readyz with no version returned %d, want 503", rec.Code)
	}

	// Liveness must still pass, or Kubernetes restarts a pod that is merely
	// waiting for its first bundle.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("healthz returned %d with no version, want 200", rec.Code)
	}

	loaded := newTestServer(t)
	rec = httptest.NewRecorder()
	loaded.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("readyz with a version returned %d, want 200", rec.Code)
	}
}
