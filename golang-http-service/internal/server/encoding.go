package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// encode writes v as JSON with the given status code.
// r is accepted (article signature) so future changes can do
// content negotiation without touching every caller.
func encode[T any](w http.ResponseWriter, r *http.Request, status int, v T) error {
	_ = r
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		return fmt.Errorf("encode json: %w", err)
	}
	return nil
}

// decode reads JSON from the request body into a value of T.
func decode[T any](r *http.Request) (T, error) {
	var v T
	if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
		return v, fmt.Errorf("decode json: %w", err)
	}
	return v, nil
}

// decodeValid is the article's explicit, generic form: T must implement
// Validator, and the value is rejected if Valid returns any problems.
func decodeValid[T Validator](r *http.Request) (T, map[string]string, error) {
	var v T
	if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
		return v, nil, fmt.Errorf("decode json: %w", err)
	}
	if problems := v.Valid(r.Context()); len(problems) > 0 {
		return v, problems, fmt.Errorf("invalid %T: %d problems", v, len(problems))
	}
	return v, nil, nil
}

// Validator is implemented by request bodies that want self-validation
// after JSON decoding. A non-empty problems map signals invalid input;
// keys are field names, values are human-readable explanations.
type Validator interface {
	Valid(ctx context.Context) (problems map[string]string)
}
