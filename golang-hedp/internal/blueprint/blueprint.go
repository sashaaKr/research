// Package blueprint models the release graph: which Helm releases exist, what
// gates them on, what they depend on, and what they publish to their
// dependents.
//
// Dependencies here are between *releases*, not between charts. Helm's own
// subchart dependencies are resolved inside a single render; these edges span
// renders, which is what makes ordering (and therefore parallelism) a real
// question rather than a detail.
package blueprint

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
)

// Release is one node in the graph.
type Release struct {
	Name      string `json:"name"`
	Chart     string `json:"chart"`
	Namespace string `json:"namespace"`
	// Requires lists customer features that must all be enabled for this
	// release to be selected on its own. An empty list means "always on".
	Requires []string `json:"requires,omitempty"`
	// DependsOn lists releases that must render first. A dependency is pulled
	// into the plan even when its own Requires are not satisfied - that is the
	// entire point of an explicit edge.
	DependsOn []string `json:"dependsOn,omitempty"`
	// Values are the static per-release values, merged under the customer's.
	Values map[string]any `json:"values,omitempty"`
	// Exports are tiny templates evaluated after this release renders. The
	// results are handed to dependents under .Values.deps.<release>.
	Exports map[string]string `json:"exports,omitempty"`
}

// Blueprint is the whole graph, validated and topologically ordered.
type Blueprint struct {
	Releases []Release `json:"releases"`

	byName map[string]*Release
	// order is a global topological order; any subset of it is a valid order
	// for that subset.
	order map[string]int
	// dependents is the reverse index, used to explain impact.
	dependents map[string][]string
}

// Load reads and validates a blueprint file.
func Load(path string) (*Blueprint, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read blueprint: %w", err)
	}
	return Parse(data)
}

// Parse validates a blueprint that is already in memory - the path taken when
// a versioned release set arrives as a bundle rather than a directory.
func Parse(data []byte) (*Blueprint, error) {
	var bp Blueprint
	if err := yaml.Unmarshal(data, &bp); err != nil {
		return nil, fmt.Errorf("parse blueprint: %w", err)
	}
	if err := bp.index(); err != nil {
		return nil, err
	}
	return &bp, nil
}

func (b *Blueprint) index() error {
	if len(b.Releases) == 0 {
		return fmt.Errorf("blueprint has no releases")
	}
	b.byName = make(map[string]*Release, len(b.Releases))
	b.dependents = make(map[string][]string, len(b.Releases))
	for i := range b.Releases {
		r := &b.Releases[i]
		if r.Name == "" {
			return fmt.Errorf("release %d has no name", i)
		}
		if r.Chart == "" {
			return fmt.Errorf("release %q has no chart", r.Name)
		}
		if _, dup := b.byName[r.Name]; dup {
			return fmt.Errorf("duplicate release %q", r.Name)
		}
		if r.Namespace == "" {
			r.Namespace = "default"
		}
		b.byName[r.Name] = r
	}
	for i := range b.Releases {
		r := &b.Releases[i]
		for _, dep := range r.DependsOn {
			if _, ok := b.byName[dep]; !ok {
				return fmt.Errorf("release %q depends on unknown release %q", r.Name, dep)
			}
			b.dependents[dep] = append(b.dependents[dep], r.Name)
		}
	}
	order, err := b.topoSort()
	if err != nil {
		return err
	}
	b.order = order
	return nil
}

// topoSort produces a deterministic global order and rejects cycles. A cycle
// in the release graph is a configuration bug that would otherwise surface as
// a deadlock or a missing export at render time, so it is caught at load.
func (b *Blueprint) topoSort() (map[string]int, error) {
	const (
		white = 0 // unvisited
		grey  = 1 // on the current path
		black = 2 // done
	)
	state := make(map[string]int, len(b.Releases))
	order := make(map[string]int, len(b.Releases))
	var out []string

	names := make([]string, 0, len(b.Releases))
	for i := range b.Releases {
		names = append(names, b.Releases[i].Name)
	}
	sort.Strings(names)

	var visit func(name string, path []string) error
	visit = func(name string, path []string) error {
		switch state[name] {
		case black:
			return nil
		case grey:
			return fmt.Errorf("dependency cycle: %s -> %s", strings.Join(path, " -> "), name)
		}
		state[name] = grey
		deps := append([]string(nil), b.byName[name].DependsOn...)
		sort.Strings(deps)
		for _, d := range deps {
			if err := visit(d, append(path, name)); err != nil {
				return err
			}
		}
		state[name] = black
		out = append(out, name)
		return nil
	}
	for _, n := range names {
		if err := visit(n, nil); err != nil {
			return nil, err
		}
	}
	for i, n := range out {
		order[n] = i
	}
	return order, nil
}

// Get returns a release by name.
func (b *Blueprint) Get(name string) (*Release, bool) {
	r, ok := b.byName[name]
	return r, ok
}

// Names returns all release names in topological order.
func (b *Blueprint) Names() []string {
	names := make([]string, 0, len(b.Releases))
	for i := range b.Releases {
		names = append(names, b.Releases[i].Name)
	}
	sort.Slice(names, func(i, j int) bool { return b.order[names[i]] < b.order[names[j]] })
	return names
}

// Features returns every feature named by any release's Requires. The service
// validates customer configs against this set, so a typo in a feature name is
// a 422 rather than a silently smaller render.
func (b *Blueprint) Features() []string {
	seen := map[string]bool{}
	var out []string
	for i := range b.Releases {
		for _, f := range b.Releases[i].Requires {
			if !seen[f] {
				seen[f] = true
				out = append(out, f)
			}
		}
	}
	sort.Strings(out)
	return out
}

// Plan is an ordered, wave-partitioned set of releases to render.
type Plan struct {
	// Waves partitions the selected releases so that every release in wave i
	// depends only on releases in waves < i. Everything inside a wave is
	// independent and can render in parallel.
	Waves [][]string
	// Selected is the flat set, in topological order.
	Selected []string
	// PulledIn records releases that were added purely to satisfy a
	// dependency, i.e. the customer's features did not select them directly.
	PulledIn []string
}

// Count returns the number of releases in the plan.
func (p Plan) Count() int { return len(p.Selected) }

// Width returns the widest wave, an upper bound on useful parallelism.
func (p Plan) Width() int {
	w := 0
	for _, wave := range p.Waves {
		if len(wave) > w {
			w = len(wave)
		}
	}
	return w
}

// PlanFor selects the releases a customer's feature set implies and partitions
// them into dependency waves.
//
// Selection is two-phase on purpose. First the feature gates pick the directly
// requested releases; then the dependency closure pulls in whatever those need.
// Reporting the second set separately (PulledIn) is what lets a customer see
// why they are paying for a release they never asked for.
func (b *Blueprint) PlanFor(features []string, all bool) Plan {
	enabled := make(map[string]bool, len(features))
	for _, f := range features {
		enabled[f] = true
	}

	direct := make(map[string]bool, len(b.Releases))
	for i := range b.Releases {
		r := &b.Releases[i]
		if all || b.satisfied(r, enabled) {
			direct[r.Name] = true
		}
	}

	selected := make(map[string]bool, len(direct))
	var closure func(name string)
	closure = func(name string) {
		if selected[name] {
			return
		}
		selected[name] = true
		for _, d := range b.byName[name].DependsOn {
			closure(d)
		}
	}
	for name := range direct {
		closure(name)
	}

	flat := make([]string, 0, len(selected))
	for name := range selected {
		flat = append(flat, name)
	}
	sort.Slice(flat, func(i, j int) bool { return b.order[flat[i]] < b.order[flat[j]] })

	var pulledIn []string
	for _, name := range flat {
		if !direct[name] {
			pulledIn = append(pulledIn, name)
		}
	}

	return Plan{Waves: b.waves(selected, flat), Selected: flat, PulledIn: pulledIn}
}

// waves assigns each release the depth of its longest dependency chain within
// the selection. Depth is the earliest wave a release can render in, so this
// is the schedule with the shortest critical path.
func (b *Blueprint) waves(selected map[string]bool, flat []string) [][]string {
	depth := make(map[string]int, len(flat))
	maxDepth := 0
	// flat is in topological order, so every dependency's depth is already known.
	for _, name := range flat {
		d := 0
		for _, dep := range b.byName[name].DependsOn {
			if !selected[dep] {
				continue
			}
			if dd := depth[dep] + 1; dd > d {
				d = dd
			}
		}
		depth[name] = d
		if d > maxDepth {
			maxDepth = d
		}
	}
	waves := make([][]string, maxDepth+1)
	for _, name := range flat {
		waves[depth[name]] = append(waves[depth[name]], name)
	}
	return waves
}

func (b *Blueprint) satisfied(r *Release, enabled map[string]bool) bool {
	for _, f := range r.Requires {
		if !enabled[f] {
			return false
		}
	}
	return true
}

// Validate checks that every release points at a chart that actually exists.
// Called once at startup against the catalog, so a missing chart is a boot
// failure rather than a per-request 500.
func (b *Blueprint) Validate(hasChart func(string) bool) error {
	var missing []string
	for i := range b.Releases {
		if !hasChart(b.Releases[i].Chart) {
			missing = append(missing, fmt.Sprintf("%s -> %s", b.Releases[i].Name, b.Releases[i].Chart))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("blueprint references %d missing charts: %s", len(missing), strings.Join(missing, ", "))
	}
	return nil
}
