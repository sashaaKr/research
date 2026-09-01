package cachedengine

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/BurntSushi/toml"
	goYaml "go.yaml.in/yaml/v3"
	"sigs.k8s.io/yaml"
)

// Ports of the serialisation helpers from helm.sh/helm/v3/pkg/engine/funcs.go.
// They are unexported there, and their exact error-swallowing behaviour is
// observable from templates, so they are copied rather than approximated.

func toYAMLPretty(v any) string {
	var data bytes.Buffer
	encoder := goYaml.NewEncoder(&data)
	encoder.SetIndent(2)
	if err := encoder.Encode(v); err != nil {
		return ""
	}
	return strings.TrimSuffix(data.String(), "\n")
}

func fromYAML(str string) map[string]any {
	m := map[string]any{}
	if err := yaml.Unmarshal([]byte(str), &m); err != nil {
		m["Error"] = err.Error()
	}
	return m
}

func fromYAMLArray(str string) []any {
	a := []any{}
	if err := yaml.Unmarshal([]byte(str), &a); err != nil {
		a = []any{err.Error()}
	}
	return a
}

func toTOMLStub(v any) string {
	b := bytes.NewBuffer(nil)
	if err := toml.NewEncoder(b).Encode(v); err != nil {
		return err.Error()
	}
	return b.String()
}

func fromTOMLStub(str string) map[string]any {
	m := make(map[string]any)
	if err := toml.Unmarshal([]byte(str), &m); err != nil {
		m["Error"] = err.Error()
	}
	return m
}

func toJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(data)
}

func fromJSON(str string) map[string]any {
	m := make(map[string]any)
	if err := json.Unmarshal([]byte(str), &m); err != nil {
		m["Error"] = err.Error()
	}
	return m
}

func fromJSONArray(str string) []any {
	a := []any{}
	if err := json.Unmarshal([]byte(str), &a); err != nil {
		a = []any{err.Error()}
	}
	return a
}
