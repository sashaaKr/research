package render_test

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"testing"
	"text/template"

	"github.com/Masterminds/sprig/v3"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"

	"github.com/sashaakr/research/golang-hedp/internal/catalog"
	"github.com/sashaakr/research/golang-hedp/internal/customer"
	"github.com/sashaakr/research/golang-hedp/internal/render"
)

// The benchmarks answer, in order:
//
//  1. What does one customer cost, and how does that scale with how much of
//     the library they select?
//  2. Where does that time actually go - loading, values, parsing, executing?
//  3. Which parallelism knob is worth turning for a bulk request?
//
// Run: go test ./internal/render -bench . -benchtime 5x -run '^$'

func benchConfigs() []struct {
	name string
	cfg  customer.Config
} {
	return []struct {
		name string
		cfg  customer.Config
	}{
		{"minimal", customer.Config{
			ID: "bench-min", Tier: "free", Region: "eu-west-1",
		}},
		{"typical", customer.Config{
			ID: "bench-typ", Tier: "standard", Region: "eu-west-1",
			Features: []string{"observability", "mesh"},
		}},
		{"heavy", customer.Config{
			ID: "bench-hvy", Tier: "premium", Region: "us-east-1",
			Features: []string{"observability", "mesh", "search", "analytics", "cdn"},
		}},
		{"all", customer.Config{
			ID: "bench-all", Tier: "enterprise", Region: "us-east-1", RenderAll: true,
		}},
	}
}

// BenchmarkPlan measures selection + topological ordering with no rendering.
// It exists to establish that planning is free, so any latency the service
// shows is real template work rather than graph bookkeeping.
func BenchmarkPlan(b *testing.B) {
	r := newRenderer(b, render.Options{})
	bp := r.Blueprint()

	for _, tc := range benchConfigs() {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			var releases int
			for i := 0; i < b.N; i++ {
				plan := bp.PlanFor(tc.cfg.Features, tc.cfg.RenderAll)
				releases = plan.Count()
			}
			b.ReportMetric(float64(releases), "releases")
		})
	}
}

// BenchmarkRenderCustomer is the headline number: one HTTP request's worth of
// work, end to end, for a single customer.
func BenchmarkRenderCustomer(b *testing.B) {
	r := newRenderer(b, render.Options{ReleaseConcurrency: 1})
	ctx := context.Background()

	for _, tc := range benchConfigs() {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			var res render.Result
			for i := 0; i < b.N; i++ {
				res = r.Render(ctx, tc.cfg)
				if !res.OK {
					b.Fatalf("render failed: %s", firstError(res))
				}
			}
			b.ReportMetric(float64(res.Planned), "releases")
			b.ReportMetric(float64(res.Manifests), "manifests")
			b.ReportMetric(float64(res.TotalBytes)/(1<<20), "MB-out")
		})
	}
}

// BenchmarkReleaseConcurrency asks how much single-request latency is bought
// by rendering a customer's independent releases in parallel. The ceiling is
// the widest dependency wave, not the number of releases.
func BenchmarkReleaseConcurrency(b *testing.B) {
	ctx := context.Background()
	cfg := customer.Config{
		ID: "bench-conc", Tier: "premium", Region: "us-east-1", RenderAll: true,
	}

	for _, width := range []int{1, 2, 4, 8} {
		b.Run(fmt.Sprintf("width=%d", width), func(b *testing.B) {
			r := newRenderer(b, render.Options{
				ReleaseConcurrency:   width,
				MaxConcurrentRenders: width,
			})
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if res := r.Render(ctx, cfg); !res.OK {
					b.Fatalf("render failed: %s", firstError(res))
				}
			}
		})
	}
}

// BenchmarkBulk is the bulk API under its real shape: many customers, each
// rendered serially, parallelism spent across customers rather than within
// one. b.N is the number of *batches*; the per-config cost is the reported
// custom metric.
func BenchmarkBulk(b *testing.B) {
	ctx := context.Background()
	const batch = 200

	configs := syntheticConfigs(batch, 0)

	for _, conc := range []int{1, 2, 4, 8} {
		b.Run(fmt.Sprintf("concurrency=%d", conc), func(b *testing.B) {
			r := newRenderer(b, render.Options{
				ReleaseConcurrency:   1,
				MaxConcurrentRenders: conc,
			})
			b.ReportAllocs()
			b.ResetTimer()

			var summary render.BulkSummary
			for i := 0; i < b.N; i++ {
				summary = r.RenderBulk(ctx, configs, conc, nil)
				if summary.Failed > 0 || summary.Invalid > 0 {
					b.Fatalf("bulk had %d failed, %d invalid", summary.Failed, summary.Invalid)
				}
			}
			b.ReportMetric(summary.ConfigsPerSecond, "configs/s")
			b.ReportMetric(float64(summary.P99Micros)/1000, "p99-ms")
			b.ReportMetric(summary.MBPerSecond, "MB/s")
		})
	}
}

// BenchmarkBulkCache measures what memoising identical renders is worth
// *within a single batch*.
//
// The cache is reset before every iteration on purpose. Leaving it warm would
// measure the benchmark replaying the same batch into a full cache, which
// reports spectacular numbers and answers no question anyone has. The variable
// that matters is how many configs in one request are byte-identical to
// another - at 0% the cache is pure overhead, and that is worth seeing.
func BenchmarkBulkCache(b *testing.B) {
	ctx := context.Background()
	const batch = 200

	for _, dupes := range []int{0, 50, 90} {
		for _, cached := range []bool{false, true} {
			name := fmt.Sprintf("duplicates=%d%%/cache=%v", dupes, cached)
			b.Run(name, func(b *testing.B) {
				opts := render.Options{ReleaseConcurrency: 1, MaxConcurrentRenders: 4}
				cache := render.NewCache(1 << 16)
				if cached {
					opts.Cache = cache
				}
				r := newRenderer(b, opts)
				configs := syntheticConfigs(batch, dupes)

				b.ReportAllocs()
				b.ResetTimer()

				var summary render.BulkSummary
				for i := 0; i < b.N; i++ {
					b.StopTimer()
					cache.Reset()
					b.StartTimer()

					summary = r.RenderBulk(ctx, configs, 4, nil)
				}
				b.ReportMetric(summary.ConfigsPerSecond, "configs/s")
				if summary.Cache != nil {
					if total := summary.Cache.Hits + summary.Cache.Misses; total > 0 {
						b.ReportMetric(100*float64(summary.Cache.Hits)/float64(total), "hit%")
					}
				}
			})
		}
	}
}

// BenchmarkCatalogLoad measures cold start. This is the cost the service pays
// once so that no request ever pays it.
func BenchmarkCatalogLoad(b *testing.B) {
	root := filepath.Join(libraryRoot(b), "charts")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		cat, err := catalog.Load(root)
		if err != nil {
			b.Fatal(err)
		}
		b.ReportMetric(float64(cat.Stats().TotalBytes)/(1<<20), "MB-loaded")
	}
}

// --- Where the time goes -----------------------------------------------------
//
// These four benchmarks decompose a single chart render into its stages, on
// the largest chart in the library (the worst case, and the one that sets p99).

func largestChart(tb testing.TB, cat *catalog.Catalog) (string, *chart.Chart) {
	tb.Helper()
	var bestName string
	var best *chart.Chart
	var bestBytes int
	for _, name := range cat.Names() {
		c, _ := cat.Chart(name)
		n := 0
		for _, t := range c.Templates {
			n += len(t.Data)
		}
		if n > bestBytes {
			bestBytes, bestName, best = n, name, c
		}
	}
	return bestName, best
}

func benchChart(tb testing.TB) (*chart.Chart, chartutil.Values) {
	tb.Helper()
	root := libraryRoot(tb)
	cat, err := catalog.Load(filepath.Join(root, "charts"))
	if err != nil {
		tb.Fatal(err)
	}
	name, chrt := largestChart(tb, cat)
	tb.Logf("largest chart: %s", name)

	vals, err := chartutil.ToRenderValues(chrt, map[string]any{
		"customer": map[string]any{
			"id": "bench", "tier": "premium", "region": "eu-west-1",
			"features": []any{"observability"},
		},
	}, chartutil.ReleaseOptions{Name: "bench", Namespace: "default", IsInstall: true, Revision: 1}, chartutil.DefaultCapabilities)
	if err != nil {
		tb.Fatal(err)
	}
	return chrt, vals
}

// BenchmarkStageValues isolates chartutil.ToRenderValues. It deep-copies both
// the supplied values and the chart's own defaults on every call, so it is not
// free even though it looks like bookkeeping.
func BenchmarkStageValues(b *testing.B) {
	root := libraryRoot(b)
	cat, err := catalog.Load(filepath.Join(root, "charts"))
	if err != nil {
		b.Fatal(err)
	}
	_, chrt := largestChart(b, cat)
	vals := map[string]any{
		"customer": map[string]any{"id": "bench", "tier": "premium", "region": "eu-west-1"},
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := chartutil.ToRenderValues(chrt, vals, chartutil.ReleaseOptions{
			Name: "bench", Namespace: "default", IsInstall: true, Revision: 1,
		}, chartutil.DefaultCapabilities); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkStageParseOnly parses the chart's templates and does nothing else.
//
// This is the load-bearing measurement of the whole study. Helm's engine calls
// template.Parse on every file of every chart on every single Render (see
// engine.go's render()), so whatever this costs is paid per request, forever,
// no matter how well the rest of the service is written.
func BenchmarkStageParseOnly(b *testing.B) {
	root := libraryRoot(b)
	cat, err := catalog.Load(filepath.Join(root, "charts"))
	if err != nil {
		b.Fatal(err)
	}
	_, chrt := largestChart(b, cat)

	// Mirror the engine's ordering so the comparison is apples to apples.
	names := make([]string, 0, len(chrt.Templates))
	byName := make(map[string]string, len(chrt.Templates))
	for _, t := range chrt.Templates {
		names = append(names, t.Name)
		byName[t.Name] = string(t.Data)
	}
	sort.Strings(names)

	var bytes int64
	for _, n := range names {
		bytes += int64(len(byName[n]))
	}

	b.ReportAllocs()
	b.SetBytes(bytes)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		t := template.New("gotpl").Funcs(parseTimeFuncs)
		for _, name := range names {
			if _, err := t.New(name).Parse(byName[name]); err != nil {
				b.Fatal(err)
			}
		}
	}
}

// parseTimeFuncs mirrors the engine's function map closely enough for parsing.
// text/template resolves function *names* at parse time, so the map has to
// contain every name the charts call - the implementations are irrelevant here
// because nothing is executed.
var parseTimeFuncs = func() template.FuncMap {
	f := sprig.TxtFuncMap()
	for name, fn := range map[string]any{
		"toToml": func(any) string { return "" }, "fromToml": func(string) map[string]any { return nil },
		"toYaml": func(any) string { return "" }, "toYamlPretty": func(any) string { return "" },
		"fromYaml": func(string) map[string]any { return nil }, "fromYamlArray": func(string) []any { return nil },
		"toJson": func(any) string { return "" }, "fromJson": func(string) map[string]any { return nil },
		"fromJsonArray": func(string) []any { return nil },
		"include":       func(string, any) string { return "" },
		"tpl":           func(string, any) any { return "" },
		"required":      func(string, any) (any, error) { return "", nil },
		"lookup":        func(string, string, string, string) (map[string]any, error) { return nil, nil },
	} {
		f[name] = fn
	}
	return f
}()

// BenchmarkStageRender is the full engine call: parse plus execute. Subtract
// BenchmarkStageParseOnly to see what execution alone costs.
func BenchmarkStageRender(b *testing.B) {
	chrt, vals := benchChart(b)

	var bytes int64
	for _, t := range chrt.Templates {
		bytes += int64(len(t.Data))
	}

	b.ReportAllocs()
	b.SetBytes(bytes)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out, err := engine.Render(chrt, vals)
		if err != nil {
			b.Fatal(err)
		}
		if len(out) == 0 {
			b.Fatal("nothing rendered")
		}
	}
}

// syntheticConfigs builds a batch that looks like a real bulk request: a
// spread of tiers, regions and feature sets. dupePercent controls how many are
// byte-identical to an earlier config, which is the only thing the render
// cache can exploit.
func syntheticConfigs(n, dupePercent int) []customer.Config {
	tiers := customer.Tiers
	regions := []string{"eu-west-1", "us-east-1", "ap-south-1", "eu-central-2"}
	featureSets := [][]string{
		{},
		{"observability"},
		{"observability", "mesh"},
		{"observability", "mesh", "search"},
		{"analytics", "cdn"},
		{"observability", "mesh", "search", "analytics", "cdn", "ml"},
	}

	configs := make([]customer.Config, n)
	unique := 0
	for i := range configs {
		if dupePercent > 0 && i > 0 && (i*100/n) >= (100-dupePercent) {
			// Tail of the batch repeats the head verbatim.
			configs[i] = configs[i%max(unique, 1)]
			continue
		}
		configs[i] = customer.Config{
			ID:       fmt.Sprintf("cust-%05d", i),
			Tier:     tiers[i%len(tiers)],
			Region:   regions[i%len(regions)],
			Features: featureSets[i%len(featureSets)],
			Replicas: 1 + i%5,
		}
		unique++
	}
	return configs
}

// BenchmarkEngine is the payoff measurement: the same customers, the same
// pipeline, with and without the parse-once engine.
func BenchmarkEngine(b *testing.B) {
	ctx := context.Background()

	for _, cached := range []bool{false, true} {
		engineName := "helm"
		if cached {
			engineName = "cached"
		}
		r := newRenderer(b, render.Options{ReleaseConcurrency: 1, CachedEngine: cached})

		for _, tc := range benchConfigs() {
			b.Run(engineName+"/"+tc.name, func(b *testing.B) {
				b.ReportAllocs()
				b.ResetTimer()
				var res render.Result
				for i := 0; i < b.N; i++ {
					res = r.Render(ctx, tc.cfg)
					if !res.OK {
						b.Fatalf("render failed: %s", firstError(res))
					}
				}
				b.ReportMetric(float64(res.Planned), "releases")
			})
		}
	}
}

// BenchmarkBulkEngine is the same comparison for the bulk path, which is where
// the throughput number the service is judged on comes from.
func BenchmarkBulkEngine(b *testing.B) {
	ctx := context.Background()
	configs := syntheticConfigs(200, 0)

	for _, cached := range []bool{false, true} {
		engineName := "helm"
		if cached {
			engineName = "cached"
		}
		b.Run(engineName, func(b *testing.B) {
			r := newRenderer(b, render.Options{
				ReleaseConcurrency:   1,
				MaxConcurrentRenders: 4,
				CachedEngine:         cached,
			})
			b.ReportAllocs()
			b.ResetTimer()

			var summary render.BulkSummary
			for i := 0; i < b.N; i++ {
				summary = r.RenderBulk(ctx, configs, 4, nil)
			}
			b.ReportMetric(summary.ConfigsPerSecond, "configs/s")
			b.ReportMetric(float64(summary.P99Micros)/1000, "p99-ms")
		})
	}
}
