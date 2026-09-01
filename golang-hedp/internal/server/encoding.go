package server

import (
	"encoding/json"
	"fmt"
	"net/http"
)

func encode[T any](w http.ResponseWriter, status int, v T) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The status line is already out; all that is left is to stop writing.
		return
	}
}

func decode[T any](w http.ResponseWriter, r *http.Request, maxBytes int64) (T, bool) {
	var v T
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		encode(w, http.StatusBadRequest, errorResponse{
			Error: fmt.Sprintf("cannot decode request body: %v", err),
		})
		return v, false
	}
	return v, true
}

type errorResponse struct {
	Error string `json:"error"`
}
