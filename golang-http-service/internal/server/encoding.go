package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// encode writes v as JSON with the given status code.
func encode[T any](w http.ResponseWriter, status int, v T) error {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		return fmt.Errorf("encode json: %w", err)
	}
	return nil
}

// decode reads JSON from the request body into a value of T.
// If T implements Validator, Valid is called and any problems are returned.
func decode[T any](r *http.Request) (T, map[string]string, error) {
	var v T
	if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
		return v, nil, fmt.Errorf("decode json: %w", err)
	}
	if vv, ok := any(v).(Validator); ok {
		if problems := vv.Valid(r.Context()); len(problems) > 0 {
			return v, problems, fmt.Errorf("invalid %T: %d problems", v, len(problems))
		}
	}
	return v, nil, nil
}

// Validator is implemented by request bodies that want self-validation
// after JSON decoding. Returning a non-empty map signals invalid input.
type Validator interface {
	Valid(ctx context.Context) (problems map[string]string)
}
