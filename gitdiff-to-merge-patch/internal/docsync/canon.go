package docsync

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"strings"

	yaml "go.yaml.in/yaml/v3"
)

// Canonical decodes a JSON or YAML document and re-encodes it as canonical
// JSON: object keys sorted, insignificant whitespace removed.
//
// This step is not cosmetic. Git reports a file as modified when anyone
// reformats it, reorders keys, or edits a YAML comment. Without
// canonicalisation those show up as diffs and turn into pointless (and
// occasionally destructive) API calls. After canonicalisation, semantically
// equal documents are byte-equal and drop out of the plan entirely.
//
// The format is chosen from the file extension. Anything that is not
// .json/.yaml/.yml is rejected: a binary or free-text file has no meaningful
// JSON merge patch and should be filtered out upstream.
func Canonical(name string, data []byte) ([]byte, error) {
	switch strings.ToLower(path.Ext(name)) {
	case ".json":
		return canonicalJSON(data)
	case ".yaml", ".yml":
		return canonicalYAML(data)
	default:
		return nil, fmt.Errorf("docsync: %s: unsupported document format", name)
	}
}

func canonicalJSON(data []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	// UseNumber keeps integers exact. Without it every number round-trips
	// through float64 and IDs beyond 2^53 silently change value -- the kind
	// of corruption that only shows up in production data.
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("docsync: decode json: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("docsync: trailing content after json document")
	}
	// encoding/json sorts map keys on marshal, which is the canonical order.
	out, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("docsync: encode json: %w", err)
	}
	return out, nil
}

func canonicalYAML(data []byte) ([]byte, error) {
	var v any
	if err := yaml.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("docsync: decode yaml: %w", err)
	}
	converted, err := yamlToJSONValue(v)
	if err != nil {
		return nil, err
	}
	out, err := json.Marshal(converted)
	if err != nil {
		return nil, fmt.Errorf("docsync: encode json: %w", err)
	}
	return out, nil
}

// yamlToJSONValue rewrites a decoded YAML value into something json.Marshal
// accepts. YAML allows non-string mapping keys; JSON does not, so those are a
// hard error rather than a silent stringification.
func yamlToJSONValue(v any) (any, error) {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			c, err := yamlToJSONValue(val)
			if err != nil {
				return nil, err
			}
			out[k] = c
		}
		return out, nil
	case map[any]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			ks, ok := k.(string)
			if !ok {
				return nil, fmt.Errorf("docsync: yaml mapping key %v is not a string", k)
			}
			c, err := yamlToJSONValue(val)
			if err != nil {
				return nil, err
			}
			out[ks] = c
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			c, err := yamlToJSONValue(val)
			if err != nil {
				return nil, err
			}
			out[i] = c
		}
		return out, nil
	default:
		return v, nil
	}
}
