package cachedengine

import (
	"encoding/base64"
	"path"
	"strings"

	"github.com/gobwas/glob"
	"sigs.k8s.io/yaml"

	"helm.sh/helm/v3/pkg/chart"
)

// files is the `.Files` object templates see. It is a port of the unexported
// engine.files, and it must behave identically - the differential test renders
// the whole library through both engines and compares bytes.
//
// The one deliberate difference is lifetime: Helm rebuilds this map on every
// render, inside recAllTpls. Here it is built once per chart, when the program
// is compiled, and shared by every render. That is safe because every method
// has a value receiver over a map that is never written after construction.
type files map[string][]byte

func newFiles(from []*chart.File) files {
	f := make(files, len(from))
	for _, file := range from {
		f[file.Name] = file.Data
	}
	return f
}

func (f files) GetBytes(name string) []byte {
	if v, ok := f[name]; ok {
		return v
	}
	return []byte{}
}

func (f files) Get(name string) string {
	return string(f.GetBytes(name))
}

func (f files) Glob(pattern string) files {
	g, err := glob.Compile(pattern, '/')
	if err != nil {
		g, _ = glob.Compile("**")
	}
	nf := make(files)
	for name, contents := range f {
		if g.Match(name) {
			nf[name] = contents
		}
	}
	return nf
}

func (f files) AsConfig() string {
	if f == nil {
		return ""
	}
	m := make(map[string]string, len(f))
	for k, v := range f {
		m[path.Base(k)] = string(v)
	}
	return toYAML(m)
}

func (f files) AsSecrets() string {
	if f == nil {
		return ""
	}
	m := make(map[string]string, len(f))
	for k, v := range f {
		m[path.Base(k)] = base64.StdEncoding.EncodeToString(v)
	}
	return toYAML(m)
}

func (f files) Lines(path string) []string {
	if f == nil || f[path] == nil {
		return nil
	}
	s := string(f[path])
	if s[len(s)-1] == '\n' {
		s = s[:len(s)-1]
	}
	return strings.Split(s, "\n")
}

// toYAML mirrors the engine's toYaml template function, including its habit of
// swallowing marshal errors and returning an empty string.
func toYAML(v any) string {
	data, err := yaml.Marshal(v)
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(string(data), "\n")
}
