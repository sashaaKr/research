// Package cachedengine renders Helm charts without re-parsing them.
//
// Helm's own engine parses every template file of every chart on every call to
// Render (pkg/engine/engine.go, in render()). For a CLI that is invisible: you
// pay it once and exit. For a service that renders the same immutable charts
// thousands of times a second it is the single largest cost - measured at ~59%
// of render wall time on this library's largest chart.
//
// Templates are immutable once the chart is loaded, so the parse belongs at
// startup. Compile parses a chart once into a master template; Render clones
// it - which copies the template namespace and function maps but shares the
// parse trees - rebinds the two functions that must close over the clone
// ('include' and 'tpl'), and executes.
//
// # Fidelity
//
// This is a port of Helm's engine, not a reimplementation of its ideas. The
// value scoping, template ordering, `.Files` semantics, partial skipping and
// the "<no value>" fixup are copied from Helm 3.21 so that output is
// byte-identical. That equivalence is enforced by a differential test that
// renders the entire chart library through both engines and compares.
//
// # Cost
//
// It is a fork, and forks rot. Every Helm minor release is a diff to re-read
// against pkg/engine. Use it when the parse cost is actually your bottleneck -
// the benchmarks in the parent package say when - and keep the differential
// test in CI so a Helm upgrade that changes semantics fails loudly.
package cachedengine

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"text/template"

	"github.com/Masterminds/sprig/v3"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chartutil"
)

const (
	warnStartDelim   = "HELM_ERR_START"
	warnEndDelim     = "HELM_ERR_END"
	recursionMaxNums = 1000
)

var warnRegex = regexp.MustCompile(warnStartDelim + `((?s).*)` + warnEndDelim)

// Program is a chart parsed once and ready to render many times. It is safe
// for concurrent use.
type Program struct {
	master *template.Template
	// order is the execution order, matching Helm's sortTemplates.
	order []string
	// basePaths maps a template key to the .Template.BasePath it sees.
	basePaths map[string]string
	root      *scope
	strict    bool

	// TemplateBytes is what the parse would have cost per render.
	TemplateBytes int64
	Templates     int
}

// scope is one chart in the dependency tree, with everything that does not
// change between renders precomputed.
type scope struct {
	chrt     *chart.Chart
	files    files
	meta     any
	children []*scope
	keys     []string
}

// Compile parses a chart and its subcharts once.
func Compile(chrt *chart.Chart, strict bool) (*Program, error) {
	p := &Program{
		basePaths: map[string]string{},
		strict:    strict,
	}

	sources := map[string]string{}
	p.root = p.compileScope(chrt, sources)

	p.order = make([]string, 0, len(sources))
	for key := range sources {
		p.order = append(p.order, key)
	}
	sortTemplates(p.order)

	t := template.New("gotpl")
	if strict {
		t.Option("missingkey=error")
	} else {
		t.Option("missingkey=zero")
	}
	// Parse-time function map. 'include' and 'tpl' are placeholders here: they
	// must close over the template they are executed against, so they are
	// rebound on the per-render clone.
	t.Funcs(funcMap(strict))

	for _, key := range p.order {
		if _, err := t.New(key).Parse(sources[key]); err != nil {
			return nil, cleanupParseError(key, err)
		}
		p.TemplateBytes += int64(len(sources[key]))
		p.Templates++
	}
	p.master = t
	return p, nil
}

// compileScope walks the chart tree the way Helm's recAllTpls does, recording
// the parts that are fixed for the life of the chart.
func (p *Program) compileScope(c *chart.Chart, sources map[string]string) *scope {
	s := &scope{
		chrt:  c,
		files: newFiles(c.Files),
		meta: struct {
			chart.Metadata
			IsRoot bool
		}{*c.Metadata, c.IsRoot()},
	}
	for _, child := range c.Dependencies() {
		s.children = append(s.children, p.compileScope(child, sources))
	}

	parentID := c.ChartFullPath()
	for _, t := range c.Templates {
		if t == nil || !isTemplateValid(c, t.Name) {
			continue
		}
		key := path.Join(parentID, t.Name)
		sources[key] = string(t.Data)
		p.basePaths[key] = path.Join(parentID, "templates")
		s.keys = append(s.keys, key)
	}
	return s
}

// Render executes the compiled chart against a set of values.
func (p *Program) Render(vals chartutil.Values) (rendered map[string]string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("rendering template failed: %v", r)
		}
	}()

	// Clone copies the template namespace and the function maps but shares the
	// parse trees, so this is O(number of templates), not O(bytes of template).
	t, err := p.master.Clone()
	if err != nil {
		return nil, fmt.Errorf("clone template: %w", err)
	}
	if p.strict {
		t.Option("missingkey=error")
	} else {
		t.Option("missingkey=zero")
	}

	includedNames := make(map[string]int)
	t.Funcs(template.FuncMap{
		"include": includeFun(t, includedNames),
		"tpl":     tplFun(t, includedNames, p.strict),
	})

	scopeVals := make(map[string]map[string]any, len(p.order))
	p.root.instantiate(vals, scopeVals)

	rendered = make(map[string]string, len(p.order))
	for _, key := range p.order {
		// Partials are only ever reached through include; their direct output
		// is not a manifest.
		if strings.HasPrefix(path.Base(key), "_") {
			continue
		}
		v := scopeVals[key]
		v["Template"] = chartutil.Values{"Name": key, "BasePath": p.basePaths[key]}

		var buf strings.Builder
		if err := t.ExecuteTemplate(&buf, key, v); err != nil {
			return nil, cleanupExecError(key, err)
		}
		// Same workaround as Helm: missingkey=zero still emits "<no value>"
		// for types it cannot zero.
		rendered[key] = strings.ReplaceAll(buf.String(), "<no value>", "")
	}
	return rendered, nil
}

// instantiate builds the per-render value scopes. This is the part that cannot
// be cached - it closes over the caller's values - but it only allocates a
// handful of small maps per chart, which is why hoisting the parse out of it
// is worth the fork.
func (s *scope) instantiate(vals chartutil.Values, out map[string]map[string]any) map[string]any {
	subCharts := make(map[string]any, len(s.children))
	next := map[string]any{
		"Chart":        s.meta,
		"Files":        s.files,
		"Release":      vals["Release"],
		"Capabilities": vals["Capabilities"],
		"Values":       make(chartutil.Values),
		"Subcharts":    subCharts,
	}
	if s.chrt.IsRoot() {
		next["Values"] = vals["Values"]
	} else if vs, err := vals.Table("Values." + s.chrt.Name()); err == nil {
		next["Values"] = vs
	}

	for _, child := range s.children {
		subCharts[child.chrt.Name()] = child.instantiate(chartutil.Values(next), out)
	}
	for _, key := range s.keys {
		out[key] = next
	}
	return next
}

// sortTemplates orders keys the way Helm does: deepest paths first, so that a
// top-level chart's `define` overrides a subchart's definition of the same
// name. Parse order is semantics here, not aesthetics.
func sortTemplates(keys []string) {
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		ca, cb := strings.Count(a, "/"), strings.Count(b, "/")
		if ca == cb {
			return strings.Compare(b, a) == -1
		}
		return cb < ca
	})
}

func isTemplateValid(ch *chart.Chart, templateName string) bool {
	if strings.EqualFold(ch.Metadata.Type, "library") {
		return strings.HasPrefix(path.Base(templateName), "_")
	}
	return true
}

func includeFun(t *template.Template, includedNames map[string]int) func(string, any) (string, error) {
	return func(name string, data any) (string, error) {
		var buf strings.Builder
		if v, ok := includedNames[name]; ok {
			if v > recursionMaxNums {
				return "", fmt.Errorf("rendering template has a nested reference name: %s: unable to execute template", name)
			}
			includedNames[name]++
		} else {
			includedNames[name] = 1
		}
		err := t.ExecuteTemplate(&buf, name, data)
		includedNames[name]--
		return buf.String(), err
	}
}

func tplFun(parent *template.Template, includedNames map[string]int, strict bool) func(string, any) (string, error) {
	return func(tpl string, vals any) (string, error) {
		t, err := parent.Clone()
		if err != nil {
			return "", fmt.Errorf("cannot clone template: %w", err)
		}
		if strict {
			t.Option("missingkey=error")
		} else {
			t.Option("missingkey=zero")
		}
		t.Funcs(template.FuncMap{
			"include": includeFun(t, includedNames),
			"tpl":     tplFun(t, includedNames, strict),
		})
		t, err = t.New(parent.Name()).Parse(tpl)
		if err != nil {
			return "", fmt.Errorf("cannot parse template %q: %w", tpl, err)
		}
		var buf strings.Builder
		if err := t.Execute(&buf, vals); err != nil {
			return "", fmt.Errorf("error during tpl function execution for %q: %w", tpl, err)
		}
		return strings.ReplaceAll(buf.String(), "<no value>", ""), nil
	}
}

func cleanupParseError(filename string, err error) error {
	tokens := strings.Split(err.Error(), ": ")
	if len(tokens) == 1 {
		return fmt.Errorf("parse error in (%s): %s", filename, err)
	}
	return fmt.Errorf("parse error at (%s): %s", tokens[1], tokens[len(tokens)-1])
}

func cleanupExecError(filename string, err error) error {
	if _, isExecError := err.(template.ExecError); !isExecError {
		return err
	}
	tokens := strings.SplitN(err.Error(), ": ", 3)
	if len(tokens) != 3 {
		return fmt.Errorf("execution error in (%s): %s", filename, err)
	}
	if parts := warnRegex.FindStringSubmatch(tokens[2]); len(parts) >= 2 {
		return fmt.Errorf("execution error at (%s): %s", tokens[1], parts[1])
	}
	return err
}

func warnWrap(warn string) string { return warnStartDelim + warn + warnEndDelim }

// funcMap mirrors engine.funcMap. The include/tpl entries are placeholders so
// that parsing resolves the names; Render rebinds them to the clone.
func funcMap(strict bool) template.FuncMap {
	f := sprig.TxtFuncMap()

	// Helm removes the functions that are non-deterministic or that reach
	// outside the process. Keep the same list.
	delete(f, "env")
	delete(f, "expandenv")

	extra := template.FuncMap{
		"toToml":        toTOMLStub,
		"fromToml":      fromTOMLStub,
		"toYaml":        toYAML,
		"toYamlPretty":  toYAMLPretty,
		"fromYaml":      fromYAML,
		"fromYamlArray": fromYAMLArray,
		"toJson":        toJSON,
		"fromJson":      fromJSON,
		"fromJsonArray": fromJSONArray,

		"include":  func(string, any) string { return "not implemented" },
		"tpl":      func(string, any) any { return "not implemented" },
		"required": requiredFunc,
		"lookup": func(string, string, string, string) (map[string]any, error) {
			return map[string]any{}, nil
		},
		"fail": func(msg string) (string, error) { return "", fmt.Errorf("%s", warnWrap(msg)) },
		// DNS lookups are disabled, matching an engine built without EnableDNS.
		"getHostByName": func(string) string { return "" },
	}
	for k, v := range extra {
		f[k] = v
	}
	return f
}

func requiredFunc(warn string, val any) (any, error) {
	if val == nil {
		return val, fmt.Errorf("%s", warnWrap(warn))
	}
	if s, ok := val.(string); ok && s == "" {
		return val, fmt.Errorf("%s", warnWrap(warn))
	}
	return val, nil
}
