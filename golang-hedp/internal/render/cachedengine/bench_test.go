package cachedengine_test

import (
	"testing"

	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/engine"

	"github.com/sashaakr/research/golang-hedp/internal/catalog"
	"github.com/sashaakr/research/golang-hedp/internal/render/cachedengine"
)

func largest(tb testing.TB) *chart.Chart {
	tb.Helper()
	cat, err := catalog.Load(chartsDir(tb))
	if err != nil {
		tb.Fatal(err)
	}
	var best *chart.Chart
	var bestBytes int
	for _, name := range cat.Names() {
		c, _ := cat.Chart(name)
		n := 0
		for _, t := range c.Templates {
			n += len(t.Data)
		}
		if n > bestBytes {
			bestBytes, best = n, c
		}
	}
	return best
}

// BenchmarkHelmEngine and BenchmarkCachedEngine render the same chart with the
// same values. The difference between them is the parse that Helm repeats and
// this package does once.
func BenchmarkHelmEngine(b *testing.B) {
	chrt := largest(b)
	vals := renderValues(b, chrt)

	var bytes int64
	for _, t := range chrt.Templates {
		bytes += int64(len(t.Data))
	}
	b.SetBytes(bytes)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := engine.Render(chrt, vals); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCachedEngine(b *testing.B) {
	chrt := largest(b)
	vals := renderValues(b, chrt)

	prog, err := cachedengine.Compile(chrt, false)
	if err != nil {
		b.Fatal(err)
	}

	b.SetBytes(prog.TemplateBytes)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := prog.Render(vals); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkCompile is the startup cost the cache moves the parse into. It runs
// once per chart at boot, against every request that would otherwise pay it.
func BenchmarkCompile(b *testing.B) {
	chrt := largest(b)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := cachedengine.Compile(chrt, false); err != nil {
			b.Fatal(err)
		}
	}
}
