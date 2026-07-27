package server_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

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
