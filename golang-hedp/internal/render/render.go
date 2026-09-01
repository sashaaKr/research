// Package render turns a customer configuration into rendered Helm releases.
//
// The shape of the work: for one customer, select releases from the blueprint,
// order them into dependency waves, render each wave in parallel, and feed
// each release's exports to its dependents. For a bulk request, do that for
// thousands of customers at once.
//
// Two levels of parallelism are available - across releases within one
// customer, and across customers - and they optimise for different things.
// Widening releases cuts single-request latency; widening customers raises
// bulk throughput. They compete for the same cores, so a single global
// semaphore bounds total in-flight renders regardless of which knob is turned.
package render

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"sync"
	"text/template"
	"time"

	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"

	"github.com/sashaakr/research/golang-hedp/internal/blueprint"
	"github.com/sashaakr/research/golang-hedp/internal/catalog"
	"github.com/sashaakr/research/golang-hedp/internal/customer"
	"github.com/sashaakr/research/golang-hedp/internal/render/cachedengine"
)

// Options configures a Renderer.
type Options struct {
	// MaxConcurrentRenders bounds total in-flight chart renders across all
	// requests. Rendering is CPU-bound, so the useful ceiling is GOMAXPROCS;
	// going wider just adds scheduling overhead and memory pressure.
	MaxConcurrentRenders int
	// ReleaseConcurrency bounds parallelism *within* a single customer's plan.
	// 1 renders a customer's releases serially, which is what you want under
	// bulk load: the cores are already saturated by other customers, and
	// serial execution keeps peak memory to one render at a time.
	ReleaseConcurrency int
	// Strict turns unresolved template keys into errors instead of empty
	// strings. `helm template` defaults to lenient; a validation service
	// arguably should not.
	Strict bool
	// IncludeManifests returns the rendered YAML. Bulk validation does not
	// need it - the answer is "does it render", not "what did it render" - and
	// returning it for thousands of customers is how the service runs out of
	// memory.
	IncludeManifests bool
	// Cache, if set, memoises identical (chart, values) renders.
	Cache Cache
	// CachedEngine parses each chart once at startup instead of on every
	// render. Helm's engine re-parses every template file per Render call,
	// which measures at ~59% of render time on this library. The cost is a
	// fork of Helm's engine - see internal/render/cachedengine.
	CachedEngine bool
}

func (o Options) withDefaults() Options {
	if o.MaxConcurrentRenders <= 0 {
		o.MaxConcurrentRenders = runtime.GOMAXPROCS(0)
	}
	if o.ReleaseConcurrency <= 0 {
		o.ReleaseConcurrency = runtime.GOMAXPROCS(0)
	}
	return o
}

// Renderer renders customer configurations. It is safe for concurrent use.
type Renderer struct {
	cat  *catalog.Catalog
	bp   *blueprint.Blueprint
	opts Options

	// exports holds the blueprint's export templates, parsed once at startup
	// rather than per render. They are tiny, but "tiny times thousands of
	// customers times dozens of releases" is not tiny.
	exports map[string]map[string]*template.Template

	features map[string]bool
	caps     *chartutil.Capabilities
	sem      chan struct{}

	// programs holds each chart parsed once, when Options.CachedEngine is set.
	// Keyed by chart name; nil when the stock Helm engine is in use.
	programs map[string]*cachedengine.Program
	// CompileMillis is how long that parse took, i.e. the startup cost the
	// per-request path no longer pays.
	CompileMillis int64
}

// New builds a Renderer. It fails fast on a blueprint that references missing
// charts or has an unparseable export template, so those never become 500s.
func New(cat *catalog.Catalog, bp *blueprint.Blueprint, opts Options) (*Renderer, error) {
	opts = opts.withDefaults()

	if err := bp.Validate(func(name string) bool { _, ok := cat.Chart(name); return ok }); err != nil {
		return nil, err
	}

	exports := make(map[string]map[string]*template.Template)
	for _, name := range bp.Names() {
		r, _ := bp.Get(name)
		if len(r.Exports) == 0 {
			continue
		}
		parsed := make(map[string]*template.Template, len(r.Exports))
		for key, tpl := range r.Exports {
			t, err := template.New(name + "/" + key).Parse(tpl)
			if err != nil {
				return nil, fmt.Errorf("release %q export %q: %w", name, key, err)
			}
			parsed[key] = t
		}
		exports[name] = parsed
	}

	features := make(map[string]bool)
	for _, f := range bp.Features() {
		features[f] = true
	}

	r := &Renderer{
		cat:      cat,
		bp:       bp,
		opts:     opts,
		exports:  exports,
		features: features,
		caps:     chartutil.DefaultCapabilities.Copy(),
		sem:      make(chan struct{}, opts.MaxConcurrentRenders),
	}

	if opts.CachedEngine {
		if err := r.compile(); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// compile parses every chart the blueprint can reach, once. A chart that fails
// to parse is a boot failure: better to refuse to start than to serve a
// endpoint that 500s on the release nobody tested.
func (r *Renderer) compile() error {
	start := time.Now()

	names := make([]string, 0, len(r.bp.Releases))
	seen := map[string]bool{}
	for i := range r.bp.Releases {
		if c := r.bp.Releases[i].Chart; !seen[c] {
			seen[c] = true
			names = append(names, c)
		}
	}

	var (
		mu       sync.Mutex
		firstErr error
		wg       sync.WaitGroup
	)
	programs := make(map[string]*cachedengine.Program, len(names))
	sem := make(chan struct{}, runtime.GOMAXPROCS(0))

	for _, name := range names {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			chrt, ok := r.cat.Chart(name)
			if !ok {
				mu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("compile %s: chart not found", name)
				}
				mu.Unlock()
				return
			}
			prog, err := cachedengine.Compile(chrt, r.opts.Strict)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("compile %s: %w", name, err)
				}
				return
			}
			programs[name] = prog
		}(name)
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}

	r.programs = programs
	r.CompileMillis = time.Since(start).Milliseconds()
	return nil
}

// Features returns the feature names the blueprint knows about.
func (r *Renderer) Features() map[string]bool { return r.features }

// Blueprint exposes the graph for planning-only endpoints.
func (r *Renderer) Blueprint() *blueprint.Blueprint { return r.bp }

// Options returns the effective options.
func (r *Renderer) Options() Options { return r.opts }

// Verbose returns a copy of r that also returns rendered manifests.
//
// The copy shares the catalog, the compiled programs, the parsed export
// templates and - importantly - the global admission semaphore, so it costs
// nothing to make and cannot escape the process-wide concurrency bound. Only
// IncludeManifests may differ: Strict and CachedEngine are baked into the
// compiled programs at startup, so changing them here would silently render
// against templates parsed under different rules.
func (r *Renderer) Verbose() *Renderer {
	cp := *r
	cp.opts.IncludeManifests = true
	return &cp
}

// ReleaseResult is the outcome for a single release.
type ReleaseResult struct {
	Name      string `json:"name"`
	Chart     string `json:"chart"`
	Namespace string `json:"namespace"`

	Manifests int    `json:"manifests"`
	Bytes     int    `json:"bytes"`
	Digest    string `json:"digest,omitempty"`
	Micros    int64  `json:"micros"`
	Cached    bool   `json:"cached,omitempty"`

	Error   string `json:"error,omitempty"`
	Skipped string `json:"skipped,omitempty"`

	// Files is only populated when Options.IncludeManifests is set.
	Files map[string]string `json:"files,omitempty"`
}

// Result is the outcome for one customer configuration.
type Result struct {
	CustomerID string `json:"customer_id"`
	OK         bool   `json:"ok"`

	// Problems is set when validation rejected the config; nothing was
	// rendered in that case.
	Problems map[string]string `json:"problems,omitempty"`

	Releases []ReleaseResult `json:"releases,omitempty"`

	Planned    int   `json:"planned"`
	PulledIn   int   `json:"pulled_in"`
	Waves      int   `json:"waves"`
	Manifests  int   `json:"manifests"`
	TotalBytes int   `json:"total_bytes"`
	Micros     int64 `json:"micros"`
	Failed     int   `json:"failed"`
	CacheHits  int   `json:"cache_hits"`
}

// Render validates and renders one customer configuration.
func (r *Renderer) Render(ctx context.Context, cfg customer.Config) Result {
	start := time.Now()
	res := Result{CustomerID: cfg.ID}

	if problems := cfg.Valid(r.features); len(problems) > 0 {
		res.Problems = problems
		res.Micros = time.Since(start).Microseconds()
		return res
	}

	plan := r.bp.PlanFor(cfg.Features, cfg.RenderAll)
	res.Planned = plan.Count()
	res.PulledIn = len(plan.PulledIn)
	res.Waves = len(plan.Waves)

	var (
		mu      sync.Mutex
		exports = make(map[string]map[string]string, plan.Count())
		failed  = make(map[string]string, 4)
		results = make([]ReleaseResult, 0, plan.Count())
	)

	for _, wave := range plan.Waves {
		if ctx.Err() != nil {
			break
		}

		// Releases whose upstream failed are not rendered: their values would
		// be missing the exports they were written against, so the errors
		// would be noise. Report the cause instead.
		var runnable []string
		for _, name := range wave {
			rel, _ := r.bp.Get(name)
			if blocked := firstFailed(rel.DependsOn, failed); blocked != "" {
				results = append(results, ReleaseResult{
					Name: name, Chart: rel.Chart, Namespace: rel.Namespace,
					Skipped: "dependency " + blocked + " failed",
				})
				failed[name] = blocked
				continue
			}
			runnable = append(runnable, name)
		}

		width := r.opts.ReleaseConcurrency
		if width > len(runnable) {
			width = len(runnable)
		}

		var wg sync.WaitGroup
		gate := make(chan struct{}, max(width, 1))
		for _, name := range runnable {
			wg.Add(1)
			go func(name string) {
				defer wg.Done()
				gate <- struct{}{}
				defer func() { <-gate }()

				// Snapshot the upstream exports this release needs. Waves are
				// barriers, so everything it depends on is already final.
				rel, _ := r.bp.Get(name)
				mu.Lock()
				deps := make(map[string]any, len(rel.DependsOn))
				for _, d := range rel.DependsOn {
					deps[d] = toAny(exports[d])
				}
				mu.Unlock()

				rr, exp := r.renderRelease(ctx, rel, cfg, deps)

				mu.Lock()
				results = append(results, rr)
				if rr.Error != "" {
					failed[name] = name
				} else {
					exports[name] = exp
				}
				mu.Unlock()
			}(name)
		}
		wg.Wait()
	}

	sort.Slice(results, func(i, j int) bool { return results[i].Name < results[j].Name })
	for _, rr := range results {
		res.Manifests += rr.Manifests
		res.TotalBytes += rr.Bytes
		if rr.Cached {
			res.CacheHits++
		}
		if rr.Error != "" || rr.Skipped != "" {
			res.Failed++
		}
	}
	res.Releases = results
	res.OK = res.Failed == 0 && ctx.Err() == nil
	res.Micros = time.Since(start).Microseconds()
	return res
}

func firstFailed(deps []string, failed map[string]string) string {
	for _, d := range deps {
		if _, bad := failed[d]; bad {
			return d
		}
	}
	return ""
}

// renderRelease is the hot path: everything else in this package exists to
// decide how often and in what order this function runs.
func (r *Renderer) renderRelease(ctx context.Context, rel *blueprint.Release, cfg customer.Config, deps map[string]any) (ReleaseResult, map[string]string) {
	start := time.Now()
	out := ReleaseResult{Name: rel.Name, Chart: rel.Chart, Namespace: rel.Namespace}

	chrt, ok := r.cat.Chart(rel.Chart)
	if !ok {
		out.Error = "chart not found: " + rel.Chart
		return out, nil
	}

	vals := r.buildValues(rel, cfg, deps)

	// Global admission: bounds total concurrent renders no matter how the
	// per-request knobs are set.
	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	case <-ctx.Done():
		out.Error = "cancelled before render"
		return out, nil
	}

	var key string
	if r.opts.Cache != nil {
		key = cacheKey(rel.Chart, rel.Name, rel.Namespace, vals)
		if hit, found := r.opts.Cache.Get(key); found {
			hit.Name, hit.Chart, hit.Namespace = rel.Name, rel.Chart, rel.Namespace
			hit.Cached = true
			hit.Micros = time.Since(start).Microseconds()
			return hit, r.evalExports(rel, cfg, vals)
		}
	}

	renderVals, err := chartutil.ToRenderValues(chrt, vals, chartutil.ReleaseOptions{
		Name:      cfg.ID + "-" + rel.Name,
		Namespace: rel.Namespace,
		IsInstall: true,
		Revision:  1,
	}, r.caps)
	if err != nil {
		out.Error = "values: " + err.Error()
		out.Micros = time.Since(start).Microseconds()
		return out, nil
	}

	var files map[string]string
	if prog, ok := r.programs[rel.Chart]; ok {
		files, err = prog.Render(renderVals)
	} else {
		files, err = engine.Engine{Strict: r.opts.Strict}.Render(chrt, renderVals)
	}
	if err != nil {
		out.Error = trimErr(err.Error())
		out.Micros = time.Since(start).Microseconds()
		return out, nil
	}

	// Drop NOTES.txt and anything that rendered to nothing. Whitespace-only
	// output is how a `{{- if }}`-gated template says "not for this customer",
	// and counting those as manifests would make every plan look identical.
	names := make([]string, 0, len(files))
	for name, body := range files {
		if strings.HasSuffix(name, "NOTES.txt") || strings.TrimSpace(body) == "" {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)

	h := sha256.New()
	for _, name := range names {
		out.Bytes += len(files[name])
		h.Write([]byte(name))
		h.Write([]byte(files[name]))
	}
	out.Manifests = len(names)
	out.Digest = hex.EncodeToString(h.Sum(nil))[:16]

	if r.opts.IncludeManifests {
		out.Files = make(map[string]string, len(names))
		for _, name := range names {
			out.Files[name] = files[name]
		}
	}
	out.Micros = time.Since(start).Microseconds()

	if r.opts.Cache != nil {
		// Cache the verdict, never the payload: the manifests for a 55 MB
		// library dwarf anything a cache should hold.
		r.opts.Cache.Put(key, ReleaseResult{
			Manifests: out.Manifests, Bytes: out.Bytes, Digest: out.Digest,
		})
	}

	return out, r.evalExports(rel, cfg, vals)
}

// buildValues layers the value sources. Order is the contract: blueprint
// defaults first, then values derived from the customer, then upstream
// exports, then the customer's explicit overrides last so they always win.
func (r *Renderer) buildValues(rel *blueprint.Release, cfg customer.Config, deps map[string]any) map[string]any {
	vals := make(map[string]any, len(rel.Values)+4)
	mergeInto(vals, rel.Values)

	vals["customer"] = map[string]any{
		"id":       cfg.ID,
		"tier":     cfg.Tier,
		"region":   cfg.Region,
		"features": toAnySlice(cfg.Features),
	}
	if cfg.Replicas > 0 {
		vals["replicaCount"] = cfg.Replicas
	}
	if len(deps) > 0 {
		vals["deps"] = deps
	}
	mergeInto(vals, cfg.Overrides[rel.Name])
	return vals
}

func (r *Renderer) evalExports(rel *blueprint.Release, cfg customer.Config, vals map[string]any) map[string]string {
	tpls := r.exports[rel.Name]
	if len(tpls) == 0 {
		return nil
	}
	ctxVals := map[string]any{
		"Release":  map[string]any{"Name": cfg.ID + "-" + rel.Name, "Namespace": rel.Namespace},
		"Values":   vals,
		"Customer": cfg,
	}
	out := make(map[string]string, len(tpls))
	for key, t := range tpls {
		var b strings.Builder
		if err := t.Execute(&b, ctxVals); err != nil {
			out[key] = ""
			continue
		}
		out[key] = b.String()
	}
	return out
}

// mergeInto deep-merges src into dst: maps merge, everything else replaces.
// This mirrors Helm's own coalescing rules so that overriding a value here
// behaves the way a chart author would expect.
func mergeInto(dst map[string]any, src map[string]any) {
	for k, v := range src {
		if sub, ok := v.(map[string]any); ok {
			if existing, ok := dst[k].(map[string]any); ok {
				mergeInto(existing, sub)
				continue
			}
			cp := make(map[string]any, len(sub))
			mergeInto(cp, sub)
			dst[k] = cp
			continue
		}
		dst[k] = v
	}
}

func toAny(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func toAnySlice(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}

// trimErr keeps rendering errors readable. Helm wraps template errors with the
// full chart path and line context, which is useful once and unusable when
// four thousand customers hit the same bad template.
func trimErr(s string) string {
	if i := strings.Index(s, "\n"); i > 0 {
		s = s[:i]
	}
	const limit = 300
	if len(s) > limit {
		return s[:limit] + "..."
	}
	return s
}
