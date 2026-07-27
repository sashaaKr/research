// Package library holds several versions of the chart library in RAM at once,
// keyed by revision.
//
// The requirement this exists for: a release set corresponds to a commit. A
// request has to be rendered against the charts as they were at that commit,
// not against whatever happens to be on disk. Serving that from the filesystem
// means either one checkout per process (so one version per process) or a
// checkout per request (so the render pays for I/O it should never see).
//
// Neither is necessary. A version arrives as a gzipped tar - `git archive
// <sha>`, an OCI layer, an object-store blob - and is loaded straight out of
// the byte slice it arrived in. After that it is immutable and shared by every
// request that names it.
//
// The cost is memory, and memory is the whole design constraint here: see
// Version.Footprint and the registry's budget.
package library

import (
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/sashaakr/research/golang-hedp/internal/blueprint"
	"github.com/sashaakr/research/golang-hedp/internal/catalog"
	"github.com/sashaakr/research/golang-hedp/internal/render"
)

// Version is one immutable snapshot of the chart library and its release
// graph, as of one revision.
type Version struct {
	Revision  string
	Catalog   *catalog.Catalog
	Blueprint *blueprint.Blueprint
	Renderer  *render.Renderer

	// BundleBytes is the compressed size it arrived as.
	BundleBytes int64
	// Footprint estimates resident heap for this version. See estimateHeap.
	Footprint int64

	LoadedAt   time.Time
	LoadMillis int64

	// lastUsed drives eviction. Guarded by the registry's mutex.
	lastUsed time.Time
	uses     int64
}

// Stats summarises a version for the API.
type Stats struct {
	Revision    string        `json:"revision"`
	Charts      int           `json:"charts"`
	Releases    int           `json:"releases"`
	RawBytes    int64         `json:"raw_bytes"`
	BundleBytes int64         `json:"bundle_bytes"`
	Footprint   int64         `json:"footprint_bytes"`
	LoadMillis  int64         `json:"load_millis"`
	LoadedAt    time.Time     `json:"loaded_at"`
	Uses        int64         `json:"uses"`
	Catalog     catalog.Stats `json:"catalog"`
}

func (v *Version) Stats() Stats {
	return Stats{
		Revision:    v.Revision,
		Charts:      v.Catalog.Stats().Charts,
		Releases:    len(v.Blueprint.Releases),
		RawBytes:    v.Catalog.Stats().TotalBytes,
		BundleBytes: v.BundleBytes,
		Footprint:   v.Footprint,
		LoadMillis:  v.LoadMillis,
		LoadedAt:    v.LoadedAt,
		Uses:        v.uses,
		Catalog:     v.Catalog.Stats(),
	}
}

// Heap-per-raw-byte multipliers, measured on this library (55 MB raw ->
// 58.6 MB of charts, +74.9 MB of compiled programs). They are estimates used
// for admission control, not accounting: the point is to refuse a load that
// would not fit rather than to predict the heap profile exactly.
const (
	chartHeapFactor   = 1.10
	programHeapFactor = 1.40
)

func estimateHeap(rawBytes int64, compiled bool) int64 {
	f := chartHeapFactor
	if compiled {
		f += programHeapFactor
	}
	return int64(float64(rawBytes) * f)
}

// Registry keeps a bounded set of versions resident. It is safe for concurrent
// use; loads are serialised, lookups are not.
type Registry struct {
	mu       sync.RWMutex
	versions map[string]*Version
	// loading deduplicates concurrent loads of the same revision, so a burst
	// of requests for a cold version costs one load rather than one per
	// caller - which at ~60 MB a time is the difference between a slow request
	// and an OOM.
	loading map[string]*loadGate

	budget   int64
	opts     render.Options
	fallback string
}

type loadGate struct {
	done sync.WaitGroup
	ver  *Version
	err  error
}

// Config configures a Registry.
type Config struct {
	// MemoryBudget caps the estimated heap across all resident versions.
	// Loading past it evicts the least recently used versions first.
	MemoryBudget int64
	// RenderOptions are applied to every version's renderer.
	RenderOptions render.Options
}

// NewRegistry returns an empty registry.
func NewRegistry(cfg Config) *Registry {
	if cfg.MemoryBudget <= 0 {
		cfg.MemoryBudget = 2 << 30
	}
	return &Registry{
		versions: map[string]*Version{},
		loading:  map[string]*loadGate{},
		budget:   cfg.MemoryBudget,
		opts:     cfg.RenderOptions,
	}
}

// Get returns a resident version and marks it used.
func (r *Registry) Get(revision string) (*Version, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.versions[revision]
	if !ok {
		return nil, false
	}
	v.lastUsed = time.Now()
	v.uses++
	return v, true
}

// Default returns the version to use when a request names none. It is the most
// recently loaded one, which is the useful default for a service that is fed
// versions as commits land.
func (r *Registry) Default() (*Version, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.fallback == "" {
		return nil, false
	}
	v, ok := r.versions[r.fallback]
	return v, ok
}

// SetDefault pins which revision unqualified requests render against.
func (r *Registry) SetDefault(revision string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.versions[revision]; !ok {
		return fmt.Errorf("revision %s is not resident", revision)
	}
	r.fallback = revision
	return nil
}

// LoadBundle loads a versioned library from a gzipped tar held in memory.
//
// Concurrent calls for the same revision collapse into one load; the rest wait
// and share the result. Re-loading a revision that is already resident is a
// no-op, because a revision is by definition immutable - if the bytes changed,
// it is a different revision.
func (r *Registry) LoadBundle(revision string, bundle []byte) (*Version, error) {
	if revision == "" {
		return nil, fmt.Errorf("revision must not be empty")
	}

	r.mu.Lock()
	if v, ok := r.versions[revision]; ok {
		v.lastUsed = time.Now()
		r.mu.Unlock()
		return v, nil
	}
	if gate, ok := r.loading[revision]; ok {
		r.mu.Unlock()
		gate.done.Wait()
		return gate.ver, gate.err
	}
	gate := &loadGate{}
	gate.done.Add(1)
	r.loading[revision] = gate
	r.mu.Unlock()

	v, err := r.build(revision, bundle)

	r.mu.Lock()
	delete(r.loading, revision)
	if err == nil {
		if err = r.admit(v.Footprint); err == nil {
			r.versions[revision] = v
			v.lastUsed = time.Now()
			if r.fallback == "" {
				r.fallback = revision
			}
		} else {
			err = fmt.Errorf("revision %s: %w", revision, err)
			v = nil
		}
	}
	r.mu.Unlock()

	gate.ver, gate.err = v, err
	gate.done.Done()
	return v, err
}

// LoadDirectory loads a version from a checkout on disk.
//
// It exists because the directory path is genuinely faster - reading 814 files
// from page cache in parallel beats decompressing the same bytes on one core -
// so a service that has a checkout at startup should use it. Versions pushed
// later still arrive as bundles, because a running service has no checkout to
// read.
func (r *Registry) LoadDirectory(revision, dir string) (*Version, error) {
	if revision == "" {
		return nil, fmt.Errorf("revision must not be empty")
	}

	r.mu.Lock()
	if v, ok := r.versions[revision]; ok {
		v.lastUsed = time.Now()
		r.mu.Unlock()
		return v, nil
	}
	r.mu.Unlock()

	start := time.Now()
	cat, err := catalog.Load(filepath.Join(dir, "charts"))
	if err != nil {
		return nil, fmt.Errorf("revision %s: %w", revision, err)
	}
	bp, err := blueprint.Load(filepath.Join(dir, "blueprint.yaml"))
	if err != nil {
		return nil, fmt.Errorf("revision %s: %w", revision, err)
	}
	renderer, err := render.New(cat, bp, r.opts)
	if err != nil {
		return nil, fmt.Errorf("revision %s: %w", revision, err)
	}

	v := &Version{
		Revision:   revision,
		Catalog:    cat,
		Blueprint:  bp,
		Renderer:   renderer,
		Footprint:  estimateHeap(cat.Stats().TotalBytes, r.opts.CachedEngine),
		LoadedAt:   time.Now(),
		LoadMillis: time.Since(start).Milliseconds(),
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.admit(v.Footprint); err != nil {
		return nil, fmt.Errorf("revision %s: %w", revision, err)
	}
	r.versions[revision] = v
	v.lastUsed = time.Now()
	if r.fallback == "" {
		r.fallback = revision
	}
	return v, nil
}

func (r *Registry) build(revision string, bundle []byte) (*Version, error) {
	start := time.Now()

	cat, err := catalog.LoadBundle(bundle)
	if err != nil {
		return nil, fmt.Errorf("revision %s: %w", revision, err)
	}
	bpData, err := catalog.BundleBlueprint(bundle)
	if err != nil {
		return nil, fmt.Errorf("revision %s: %w", revision, err)
	}
	bp, err := blueprint.Parse(bpData)
	if err != nil {
		return nil, fmt.Errorf("revision %s: %w", revision, err)
	}
	renderer, err := render.New(cat, bp, r.opts)
	if err != nil {
		return nil, fmt.Errorf("revision %s: %w", revision, err)
	}

	return &Version{
		Revision:    revision,
		Catalog:     cat,
		Blueprint:   bp,
		Renderer:    renderer,
		BundleBytes: int64(len(bundle)),
		Footprint:   estimateHeap(cat.Stats().TotalBytes, r.opts.CachedEngine),
		LoadedAt:    time.Now(),
		LoadMillis:  time.Since(start).Milliseconds(),
	}, nil
}

// admit makes room for incoming by evicting least-recently-used versions, and
// reports whether it succeeded. The caller must hold the write lock.
//
// It refuses rather than overshooting. The default revision is never evicted -
// dropping it would silently redirect every unqualified request to a different
// commit - so a budget that cannot hold the default plus one more version is a
// configuration error, and the error says so instead of quietly exceeding the
// limit the operator set.
//
// Eviction only removes the registry's reference. A request already holding
// the *Version keeps rendering against it and the memory is reclaimed when
// that request finishes, which is why versions are immutable values rather
// than something with a Close method.
func (r *Registry) admit(incoming int64) error {
	if incoming > r.budget {
		return fmt.Errorf("version needs %d MB but the whole budget is %d MB",
			incoming>>20, r.budget>>20)
	}

	used := int64(0)
	for _, v := range r.versions {
		used += v.Footprint
	}

	for used+incoming > r.budget {
		var oldest *Version
		for _, v := range r.versions {
			if v.Revision == r.fallback {
				continue
			}
			if oldest == nil || v.lastUsed.Before(oldest.lastUsed) {
				oldest = v
			}
		}
		if oldest == nil {
			// Only the pinned default is left and it still does not fit.
			return fmt.Errorf(
				"version needs %d MB, budget is %d MB, and %d MB is pinned by the default revision %s: raise the budget or change the default",
				incoming>>20, r.budget>>20, used>>20, r.fallback)
		}
		used -= oldest.Footprint
		delete(r.versions, oldest.Revision)
	}
	return nil
}

// Evict drops a revision explicitly.
func (r *Registry) Evict(revision string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.versions[revision]; !ok {
		return false
	}
	delete(r.versions, revision)
	if r.fallback == revision {
		r.fallback = ""
		// Promote the most recently used survivor so the service keeps a
		// default rather than starting to 400 on unqualified requests.
		var newest *Version
		for _, v := range r.versions {
			if newest == nil || v.lastUsed.After(newest.lastUsed) {
				newest = v
			}
		}
		if newest != nil {
			r.fallback = newest.Revision
		}
	}
	return true
}

// Resident lists the loaded versions, newest first.
func (r *Registry) Resident() []Stats {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]Stats, 0, len(r.versions))
	for _, v := range r.versions {
		out = append(out, v.Stats())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LoadedAt.After(out[j].LoadedAt) })
	return out
}

// Usage reports the estimated heap across resident versions and the budget.
func (r *Registry) Usage() (used, budget int64, versions int) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, v := range r.versions {
		used += v.Footprint
	}
	return used, r.budget, len(r.versions)
}

// DefaultRevision returns the revision unqualified requests use.
func (r *Registry) DefaultRevision() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.fallback
}
