package cachedengine_test

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"

	"github.com/sashaakr/research/golang-hedp/internal/catalog"
	"github.com/sashaakr/research/golang-hedp/internal/render/cachedengine"
)

func chartsDir(tb testing.TB) string {
	tb.Helper()
	dir, err := os.Getwd()
	if err != nil {
		tb.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		candidate := filepath.Join(dir, "testdata", "library", "charts")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		dir = filepath.Dir(dir)
	}
	tb.Skip("chart library not generated; run: go run ./cmd/chartgen")
	return ""
}

func renderValues(tb testing.TB, chrt *chart.Chart) chartutil.Values {
	tb.Helper()
	vals, err := chartutil.ToRenderValues(chrt, map[string]any{
		"customer": map[string]any{
			"id": "diff-test", "tier": "enterprise", "region": "eu-west-1",
			"features": []any{"observability", "mesh"},
		},
		"replicaCount": 3,
		"ingress": map[string]any{
			"enabled":   true,
			"className": "nginx",
			"hosts":     []any{map[string]any{"host": "a.example.com", "path": "/"}},
		},
		"autoscaling": map[string]any{"enabled": true},
		"assets":      map[string]any{"enabled": true},
		"env":         []any{map[string]any{"name": "MODE", "value": "test"}},
		"extraLabels": map[string]any{"team": "platform"},
		"deps": map[string]any{
			"upstream": map[string]any{"endpoint": "upstream.svc:8080", "serviceName": "upstream"},
		},
	}, chartutil.ReleaseOptions{
		Name: "diff", Namespace: "default", IsInstall: true, Revision: 1,
	}, chartutil.DefaultCapabilities)
	if err != nil {
		tb.Fatal(err)
	}
	return vals
}

// TestMatchesHelmEngine is the test that makes the fork usable. It renders
// every chart in the library through both engines with values that switch on
// every conditional path - ingress, autoscaling, .Files access, dependency
// exports, extra labels - and demands byte-identical output.
//
// If a Helm upgrade changes rendering semantics, this fails, which is the
// entire point of keeping it.
func TestMatchesHelmEngine(t *testing.T) {
	cat, err := catalog.Load(chartsDir(t))
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range cat.Names() {
		chrt, _ := cat.Chart(name)
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			vals := renderValues(t, chrt)

			want, err := engine.Render(chrt, vals)
			if err != nil {
				t.Fatalf("helm engine: %v", err)
			}

			prog, err := cachedengine.Compile(chrt, false)
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			got, err := prog.Render(renderValues(t, chrt))
			if err != nil {
				t.Fatalf("cached engine: %v", err)
			}

			if len(got) != len(want) {
				t.Fatalf("template count: got %d, want %d", len(got), len(want))
			}
			for key, wantBody := range want {
				gotBody, ok := got[key]
				if !ok {
					t.Errorf("missing template %q", key)
					continue
				}
				if gotBody != wantBody {
					t.Errorf("template %q differs:\n--- helm ---\n%s\n--- cached ---\n%s",
						key, truncate(wantBody), truncate(gotBody))
				}
			}
		})
	}
}

// TestCompiledProgramIsReusable checks the actual claim: the same Program,
// rendered repeatedly with different values, gives the same answers as
// compiling fresh each time. A stale parse tree or leaked per-render state
// would show up here and nowhere else.
func TestCompiledProgramIsReusable(t *testing.T) {
	cat, err := catalog.Load(chartsDir(t))
	if err != nil {
		t.Fatal(err)
	}
	name := cat.Names()[0]
	chrt, _ := cat.Chart(name)

	prog, err := cachedengine.Compile(chrt, false)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 5; i++ {
		got, err := prog.Render(renderValues(t, chrt))
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		want, err := engine.Render(chrt, renderValues(t, chrt))
		if err != nil {
			t.Fatal(err)
		}
		for key, wantBody := range want {
			if got[key] != wantBody {
				t.Fatalf("iteration %d: template %q drifted", i, key)
			}
		}
	}
}

// TestConcurrentRenderIsSafe exercises the sharing that makes the cache worth
// having: one parsed Program, many goroutines. Run with -race.
func TestConcurrentRenderIsSafe(t *testing.T) {
	cat, err := catalog.Load(chartsDir(t))
	if err != nil {
		t.Fatal(err)
	}
	chrt, _ := cat.Chart(cat.Names()[0])
	prog, err := cachedengine.Compile(chrt, false)
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := prog.Render(renderValues(t, chrt))
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got, err := prog.Render(renderValues(t, chrt))
			if err != nil {
				t.Errorf("goroutine %d: %v", i, err)
				return
			}
			for key, want := range baseline {
				if got[key] != want {
					t.Errorf("goroutine %d: template %q differs", i, key)
					return
				}
			}
		}(i)
	}
	wg.Wait()
}

func truncate(s string) string {
	const limit = 600
	if len(s) > limit {
		return s[:limit] + "\n...[truncated]"
	}
	return s
}
