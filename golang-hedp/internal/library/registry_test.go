package library_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sashaakr/research/golang-hedp/internal/customer"
	"github.com/sashaakr/research/golang-hedp/internal/library"
	"github.com/sashaakr/research/golang-hedp/internal/render"
)

func libraryDir(tb testing.TB) string {
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

var (
	packOnce sync.Once
	packed   []byte
	packErr  error

	packFastOnce sync.Once
	packedFast   []byte
	packFastErr  error
)

// bundle packs the library once for the whole test binary. Packing 55 MB is
// slow enough that doing it per test dominates the run.
func bundle(tb testing.TB) []byte {
	tb.Helper()
	dir := libraryDir(tb)
	packOnce.Do(func() { packed, packErr = library.Pack(dir) })
	if packErr != nil {
		tb.Fatal(packErr)
	}
	return packed
}

// packedBundle is the same library in the form a service should ship: one
// gzip per chart inside an uncompressed outer tar.
func packedBundle(tb testing.TB) []byte {
	tb.Helper()
	dir := libraryDir(tb)
	packFastOnce.Do(func() { packedFast, packFastErr = library.PackFast(dir) })
	if packFastErr != nil {
		tb.Fatal(packFastErr)
	}
	return packedFast
}

// TestPackedBundleMatchesLooseBundle checks that reorganising the bytes for
// parallel decompression does not change what gets loaded.
func TestPackedBundleMatchesLooseBundle(t *testing.T) {
	reg := library.NewRegistry(library.Config{RenderOptions: render.Options{ReleaseConcurrency: 1}})

	loose, err := reg.LoadBundle("loose", bundle(t))
	if err != nil {
		t.Fatal(err)
	}
	fast, err := reg.LoadBundle("packed", packedBundle(t))
	if err != nil {
		t.Fatal(err)
	}

	ls, fs := loose.Catalog.Stats(), fast.Catalog.Stats()
	if ls.Charts != fs.Charts || ls.TotalBytes != fs.TotalBytes {
		t.Errorf("loose %+v != packed %+v", ls, fs)
	}

	cfg := customer.Config{ID: "packing", Tier: "enterprise", Region: "eu-west-1", RenderAll: true}
	want := loose.Renderer.Render(context.Background(), cfg)
	got := fast.Renderer.Render(context.Background(), cfg)
	if !want.OK || !got.OK {
		t.Fatal("renders failed")
	}
	if len(want.Releases) != len(got.Releases) {
		t.Fatalf("release count differs: %d vs %d", len(want.Releases), len(got.Releases))
	}
	for i := range want.Releases {
		if want.Releases[i].Digest != got.Releases[i].Digest {
			t.Errorf("release %s digest differs between packings", want.Releases[i].Name)
		}
	}
	t.Logf("loose bundle %.1f MB, packed bundle %.1f MB",
		float64(len(bundle(t)))/(1<<20), float64(len(packedBundle(t)))/(1<<20))
}

func TestLoadBundleNeedsNoFilesystem(t *testing.T) {
	data := bundle(t)

	reg := library.NewRegistry(library.Config{
		RenderOptions: render.Options{ReleaseConcurrency: 1},
	})
	v, err := reg.LoadBundle("abc123", data)
	if err != nil {
		t.Fatal(err)
	}

	stats := v.Catalog.Stats()
	if stats.Charts == 0 || stats.TemplateBytes == 0 {
		t.Fatalf("bundle loaded nothing useful: %+v", stats)
	}
	if len(v.Blueprint.Releases) == 0 {
		t.Fatal("blueprint did not come out of the bundle")
	}

	// And it renders, which is the actual claim.
	res := v.Renderer.Render(context.Background(), customer.Config{
		ID: "bundle-test", Tier: "premium", Region: "eu-west-1",
		Features: []string{"observability"},
	})
	if !res.OK {
		t.Fatalf("render from in-memory bundle failed: %+v", res.Problems)
	}
	if res.Manifests == 0 {
		t.Fatal("no manifests rendered")
	}
	t.Logf("bundle %.1f MB compressed -> %.1f MB raw, loaded in %d ms, rendered %d manifests",
		float64(len(data))/(1<<20), float64(stats.TotalBytes)/(1<<20), v.LoadMillis, res.Manifests)
}

// TestBundleMatchesDirectory is the equivalence that lets the filesystem path
// be retired: the same tree, loaded either way, must render identically.
func TestBundleMatchesDirectory(t *testing.T) {
	dir := libraryDir(t)

	fromDir := newDirRenderer(t, dir)

	reg := library.NewRegistry(library.Config{RenderOptions: render.Options{ReleaseConcurrency: 1}})
	v, err := reg.LoadBundle("rev", bundle(t))
	if err != nil {
		t.Fatal(err)
	}

	cfg := customer.Config{
		ID: "equivalence", Tier: "enterprise", Region: "eu-west-1", RenderAll: true,
	}
	want := fromDir.Render(context.Background(), cfg)
	got := v.Renderer.Render(context.Background(), cfg)

	if !want.OK || !got.OK {
		t.Fatalf("renders failed: dir ok=%v, bundle ok=%v", want.OK, got.OK)
	}
	if len(want.Releases) != len(got.Releases) {
		t.Fatalf("release count differs: dir %d, bundle %d", len(want.Releases), len(got.Releases))
	}
	for i := range want.Releases {
		if want.Releases[i].Digest != got.Releases[i].Digest {
			t.Errorf("release %s: dir digest %s, bundle digest %s",
				want.Releases[i].Name, want.Releases[i].Digest, got.Releases[i].Digest)
		}
	}
}

// TestVersionsAreIsolated is the point of the whole package: two revisions
// resident at once must not see each other's charts.
func TestVersionsAreIsolated(t *testing.T) {
	base := bundle(t)
	// Pick a value the blueprint does not override - it sets replicaCount per
	// release, so a change to that default would never reach a manifest.
	modified := rewriteChartValue(t, libraryDir(t), "pullPolicy: IfNotPresent", "pullPolicy: Never")

	reg := library.NewRegistry(library.Config{RenderOptions: render.Options{ReleaseConcurrency: 1}})

	oldVer, err := reg.LoadBundle("commit-old", base)
	if err != nil {
		t.Fatal(err)
	}
	newVer, err := reg.LoadBundle("commit-new", modified)
	if err != nil {
		t.Fatal(err)
	}

	cfg := customer.Config{ID: "iso", Tier: "free", Region: "eu-west-1"}
	oldRes := oldVer.Renderer.Verbose().Render(context.Background(), cfg)
	newRes := newVer.Renderer.Verbose().Render(context.Background(), cfg)

	if !oldRes.OK || !newRes.OK {
		t.Fatal("renders failed")
	}

	// The change must show up in the new revision and not the old one.
	if !strings.Contains(allManifests(newRes), "imagePullPolicy: Never") {
		t.Error("new revision did not pick up its own chart change")
	}
	if strings.Contains(allManifests(oldRes), "imagePullPolicy: Never") {
		t.Error("old revision was contaminated by the new one's charts")
	}

	// Both must still be reachable by revision.
	if _, ok := reg.Get("commit-old"); !ok {
		t.Error("commit-old is no longer resident")
	}
	if _, ok := reg.Get("commit-new"); !ok {
		t.Error("commit-new is no longer resident")
	}
}

func TestEvictionRespectsBudget(t *testing.T) {
	data := bundle(t)

	// Room for three versions but not four, so the fourth must evict.
	reg := library.NewRegistry(library.Config{
		MemoryBudget:  200 << 20,
		RenderOptions: render.Options{ReleaseConcurrency: 1},
	})

	for _, rev := range []string{"r1", "r2", "r3", "r4"} {
		if _, err := reg.LoadBundle(rev, data); err != nil {
			t.Fatal(err)
		}
	}

	used, budget, versions := reg.Usage()
	if used > budget {
		t.Errorf("usage %d MB exceeds budget %d MB", used>>20, budget>>20)
	}
	if versions >= 4 {
		t.Errorf("expected eviction to have dropped a version, %d still resident", versions)
	}
	// The default must survive, or unqualified requests silently change commit.
	if _, ok := reg.Default(); !ok {
		t.Error("default revision was evicted")
	}
	// r1 is the default and pinned; r2 is the least recently used non-default.
	if _, ok := reg.Get("r2"); ok {
		t.Error("expected the least recently used non-default version to be evicted")
	}
	t.Logf("budget %d MB held %d versions using %d MB", budget>>20, versions, used>>20)
}

// A budget that cannot hold the pinned default plus the incoming version must
// fail loudly. Silently exceeding it would make the budget decorative, and the
// operator would find out from the OOM killer.
func TestLoadFailsWhenBudgetCannotFit(t *testing.T) {
	data := bundle(t)
	reg := library.NewRegistry(library.Config{
		MemoryBudget:  70 << 20,
		RenderOptions: render.Options{ReleaseConcurrency: 1},
	})

	if _, err := reg.LoadBundle("first", data); err != nil {
		t.Fatalf("the first version should fit: %v", err)
	}
	_, err := reg.LoadBundle("second", data)
	if err == nil {
		t.Fatal("expected the second version to be refused")
	}
	if !strings.Contains(err.Error(), "budget") {
		t.Errorf("error should explain the budget, got: %v", err)
	}

	used, budget, versions := reg.Usage()
	if used > budget {
		t.Errorf("usage %d MB exceeds budget %d MB after a refused load", used>>20, budget>>20)
	}
	if versions != 1 {
		t.Errorf("%d versions resident after a refused load, want 1", versions)
	}
}

// TestConcurrentLoadsCollapse checks the dedup: at ~60 MB a version, a burst of
// requests for a cold revision must not each load their own copy.
func TestConcurrentLoadsCollapse(t *testing.T) {
	data := bundle(t)
	reg := library.NewRegistry(library.Config{RenderOptions: render.Options{ReleaseConcurrency: 1}})

	var wg sync.WaitGroup
	versions := make([]*library.Version, 8)
	for i := range versions {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v, err := reg.LoadBundle("hot", data)
			if err != nil {
				t.Errorf("goroutine %d: %v", i, err)
				return
			}
			versions[i] = v
		}(i)
	}
	wg.Wait()

	for i := 1; i < len(versions); i++ {
		if versions[i] != versions[0] {
			t.Fatalf("goroutine %d got a different *Version - the load was not deduplicated", i)
		}
	}
	if _, _, n := reg.Usage(); n != 1 {
		t.Errorf("%d versions resident, want 1", n)
	}
}

func TestRejectsMalformedBundles(t *testing.T) {
	reg := library.NewRegistry(library.Config{})

	if _, err := reg.LoadBundle("", bundle(t)); err == nil {
		t.Error("expected an empty revision to be rejected")
	}
	if _, err := reg.LoadBundle("bad", []byte("this is not a gzip stream")); err == nil {
		t.Error("expected a non-gzip bundle to be rejected")
	}
}

func allManifests(res render.Result) string {
	var b strings.Builder
	for _, rel := range res.Releases {
		for _, body := range rel.Files {
			b.WriteString(body)
		}
	}
	return b.String()
}
