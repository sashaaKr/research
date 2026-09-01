package library_test

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/sashaakr/research/golang-hedp/internal/catalog"
	"github.com/sashaakr/research/golang-hedp/internal/library"
	"github.com/sashaakr/research/golang-hedp/internal/render"
)

// BenchmarkLoadFromDirectory and BenchmarkLoadFromBundle load the same 55 MB
// library, one by walking the filesystem and one out of a byte slice.
//
// Rendering is unaffected either way - charts are in RAM by the time a request
// arrives regardless. What the bundle path changes is how fast a *new version*
// becomes servable, which is the number that matters when every commit is a
// version.
func BenchmarkLoadFromDirectory(b *testing.B) {
	root := filepath.Join(libraryDir(b), "charts")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		cat, err := catalog.Load(root)
		if err != nil {
			b.Fatal(err)
		}
		b.SetBytes(cat.Stats().TotalBytes)
	}
}

// BenchmarkLoadFromBundle is the naive form: one gzip around the whole
// library, so decompression runs on a single core.
func BenchmarkLoadFromBundle(b *testing.B) {
	data := bundle(b)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		cat, err := catalog.LoadBundle(data)
		if err != nil {
			b.Fatal(err)
		}
		b.SetBytes(cat.Stats().TotalBytes)
	}
}

// BenchmarkLoadFromPackedBundle is the same bytes reorganised: one gzip stream
// per chart inside an uncompressed outer tar, so decompression parallelises.
func BenchmarkLoadFromPackedBundle(b *testing.B) {
	data := packedBundle(b)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		cat, err := catalog.LoadBundle(data)
		if err != nil {
			b.Fatal(err)
		}
		b.SetBytes(cat.Stats().TotalBytes)
	}
}

// BenchmarkPack measures turning a checkout into a bundle. It is not in the
// request path - in production the bundle comes from git or a registry already
// packed - but it bounds what a "load this commit" admin call costs end to end.
func BenchmarkPack(b *testing.B) {
	dir := libraryDir(b)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		data, err := library.Pack(dir)
		if err != nil {
			b.Fatal(err)
		}
		b.SetBytes(int64(len(data)))
	}
}

// BenchmarkVersionFootprint is the one that decides how many commits can be
// resident at once. It reports measured heap rather than the registry's
// estimate, so the estimate can be checked against reality.
func BenchmarkVersionFootprint(b *testing.B) {
	data := bundle(b)

	for _, compiled := range []bool{false, true} {
		name := "charts-only"
		if compiled {
			name = "charts+compiled"
		}
		b.Run(name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				reg := library.NewRegistry(library.Config{
					MemoryBudget:  8 << 30,
					RenderOptions: render.Options{ReleaseConcurrency: 1, CachedEngine: compiled},
				})

				before := heapMB()
				v, err := reg.LoadBundle("rev", data)
				if err != nil {
					b.Fatal(err)
				}
				after := heapMB()

				b.ReportMetric(after-before, "MB-heap")
				b.ReportMetric(float64(v.Footprint)/(1<<20), "MB-estimated")
				b.ReportMetric(float64(v.Catalog.Stats().TotalBytes)/(1<<20), "MB-raw")

				runtime.KeepAlive(reg)
			}
		})
	}
}

func heapMB() float64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return float64(m.HeapAlloc) / (1 << 20)
}
