package render_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sashaakr/research/golang-hedp/internal/blueprint"
	"github.com/sashaakr/research/golang-hedp/internal/catalog"
	"github.com/sashaakr/research/golang-hedp/internal/customer"
	"github.com/sashaakr/research/golang-hedp/internal/render"
)

// libraryRoot finds the generated chart library, walking up from the package
// directory. Tests skip rather than fail when it is absent: the library is a
// 55 MB build artifact, not something committed to the repository.
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

func newRenderer(tb testing.TB, opts render.Options) *render.Renderer {
	tb.Helper()
	root := libraryRoot(tb)
	cat, err := catalog.Load(filepath.Join(root, "charts"))
	if err != nil {
		tb.Fatal(err)
	}
	bp, err := blueprint.Load(filepath.Join(root, "blueprint.yaml"))
	if err != nil {
		tb.Fatal(err)
	}
	r, err := render.New(cat, bp, opts)
	if err != nil {
		tb.Fatal(err)
	}
	return r
}

func TestRenderProducesManifests(t *testing.T) {
	r := newRenderer(t, render.Options{IncludeManifests: true})

	res := r.Render(context.Background(), customer.Config{
		ID:       "acme-corp",
		Tier:     "premium",
		Region:   "eu-west-1",
		Features: []string{"observability", "mesh"},
		Replicas: 3,
	})

	if !res.OK {
		t.Fatalf("render failed: %+v", firstError(res))
	}
	if res.Planned == 0 {
		t.Fatal("no releases planned")
	}
	if res.Manifests == 0 {
		t.Fatal("no manifests rendered")
	}

	// Values must actually reach the templates - a render that silently drops
	// the customer identity would still "succeed".
	var sawCustomer bool
	for _, rel := range res.Releases {
		for _, body := range rel.Files {
			if strings.Contains(body, "hedp.example.com/customer: \"acme-corp\"") {
				sawCustomer = true
			}
		}
	}
	if !sawCustomer {
		t.Error("customer id did not reach the rendered manifests")
	}
	t.Logf("planned=%d pulled_in=%d waves=%d manifests=%d bytes=%d micros=%d",
		res.Planned, res.PulledIn, res.Waves, res.Manifests, res.TotalBytes, res.Micros)
}

// TestDependencyExportsFlow is the reason the wave scheduler exists: a
// downstream release must see the endpoint its upstream published.
func TestDependencyExportsFlow(t *testing.T) {
	r := newRenderer(t, render.Options{IncludeManifests: true})

	// Enable everything, so at least one release with dependencies renders.
	res := r.Render(context.Background(), customer.Config{
		ID: "dep-test", Tier: "enterprise", Region: "us-east-1", RenderAll: true,
	})
	if !res.OK {
		t.Fatalf("render failed: %+v", firstError(res))
	}
	if res.Waves < 2 {
		t.Fatalf("expected a multi-wave plan, got %d", res.Waves)
	}

	var sawEndpoint bool
	for _, rel := range res.Releases {
		relSpec, ok := r.Blueprint().Get(rel.Name)
		if !ok || len(relSpec.DependsOn) == 0 {
			continue
		}
		for _, body := range rel.Files {
			if strings.Contains(body, "_ENDPOINT") && strings.Contains(body, "svc.cluster.local:8080") {
				sawEndpoint = true
			}
		}
	}
	if !sawEndpoint {
		t.Error("no downstream release received an upstream export")
	}
}

func TestValidationRejectsBadConfig(t *testing.T) {
	r := newRenderer(t, render.Options{})

	res := r.Render(context.Background(), customer.Config{
		ID: "Bad_ID", Tier: "platinum", Region: "nowhere",
		Features: []string{"observability", "teleportation"},
	})
	if res.OK {
		t.Fatal("expected validation to reject the config")
	}
	for _, field := range []string{"id", "tier", "region", "features"} {
		if _, ok := res.Problems[field]; !ok {
			t.Errorf("expected a problem for %q, got %v", field, res.Problems)
		}
	}
	if len(res.Releases) != 0 {
		t.Error("nothing should render when validation fails")
	}
}

// TestConcurrentRendersAreSafe is the load-bearing assumption of the whole
// service: charts are loaded once and rendered by every request at the same
// time. Run with -race.
func TestConcurrentRendersAreSafe(t *testing.T) {
	r := newRenderer(t, render.Options{ReleaseConcurrency: 4})

	var wg sync.WaitGroup
	digests := make([]string, 16)
	for i := range digests {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res := r.Render(context.Background(), customer.Config{
				ID: "shared-input", Tier: "standard", Region: "eu-west-1",
				Features: []string{"observability"},
			})
			if !res.OK {
				t.Errorf("goroutine %d: render failed: %+v", i, firstError(res))
				return
			}
			var b strings.Builder
			for _, rel := range res.Releases {
				b.WriteString(rel.Name)
				b.WriteString(rel.Digest)
			}
			digests[i] = b.String()
		}(i)
	}
	wg.Wait()

	// Identical input must give identical output, concurrently or not.
	for i := 1; i < len(digests); i++ {
		if digests[i] != digests[0] {
			t.Fatalf("concurrent renders diverged: goroutine %d differs from 0", i)
		}
	}
}

func TestCacheReturnsSameVerdict(t *testing.T) {
	cache := render.NewCache(4096)
	r := newRenderer(t, render.Options{Cache: cache, ReleaseConcurrency: 1})

	cfg := customer.Config{
		ID: "cache-test", Tier: "standard", Region: "eu-west-1",
		Features: []string{"observability"},
	}
	first := r.Render(context.Background(), cfg)
	second := r.Render(context.Background(), cfg)

	if !first.OK || !second.OK {
		t.Fatal("renders failed")
	}
	if second.CacheHits == 0 {
		t.Fatal("second identical render hit no cache entries")
	}
	if first.Manifests != second.Manifests || first.TotalBytes != second.TotalBytes {
		t.Errorf("cached verdict differs: %d/%d vs %d/%d",
			first.Manifests, first.TotalBytes, second.Manifests, second.TotalBytes)
	}
}

func firstError(res render.Result) string {
	if len(res.Problems) > 0 {
		return "validation: " + join(res.Problems)
	}
	for _, rel := range res.Releases {
		if rel.Error != "" {
			return rel.Name + ": " + rel.Error
		}
		if rel.Skipped != "" {
			return rel.Name + ": skipped: " + rel.Skipped
		}
	}
	return "unknown"
}

func join(m map[string]string) string {
	var b strings.Builder
	for k, v := range m {
		b.WriteString(k + "=" + v + " ")
	}
	return b.String()
}

// TestCachedEngineMatchesHelm is the end-to-end version of the differential
// test in the cachedengine package: the same customer, rendered through the
// whole service pipeline with each engine, must produce identical digests for
// every release. This is what makes -cached-engine safe to turn on.
func TestCachedEngineMatchesHelm(t *testing.T) {
	stock := newRenderer(t, render.Options{ReleaseConcurrency: 2})
	cached := newRenderer(t, render.Options{ReleaseConcurrency: 2, CachedEngine: true})

	cfg := customer.Config{
		ID: "engine-diff", Tier: "enterprise", Region: "eu-west-1", RenderAll: true,
		Replicas: 4,
		Overrides: map[string]map[string]any{
			"base-00": {
				"ingress":     map[string]any{"enabled": true, "className": "nginx", "hosts": []any{map[string]any{"host": "a.example.com"}}},
				"autoscaling": map[string]any{"enabled": true},
				"assets":      map[string]any{"enabled": true},
			},
		},
	}

	want := stock.Render(context.Background(), cfg)
	got := cached.Render(context.Background(), cfg)

	if !want.OK || !got.OK {
		t.Fatalf("renders failed: stock=%s cached=%s", firstError(want), firstError(got))
	}
	if len(want.Releases) != len(got.Releases) {
		t.Fatalf("release count: got %d, want %d", len(got.Releases), len(want.Releases))
	}
	for i := range want.Releases {
		w, g := want.Releases[i], got.Releases[i]
		if w.Name != g.Name {
			t.Fatalf("release order diverged at %d: %q vs %q", i, g.Name, w.Name)
		}
		if w.Digest != g.Digest || w.Manifests != g.Manifests || w.Bytes != g.Bytes {
			t.Errorf("release %s differs: cached %s/%d/%d, helm %s/%d/%d",
				w.Name, g.Digest, g.Manifests, g.Bytes, w.Digest, w.Manifests, w.Bytes)
		}
	}
	t.Logf("both engines rendered %d releases, %d manifests, %d bytes",
		want.Planned, want.Manifests, want.TotalBytes)
}
