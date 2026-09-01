package server

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/sashaakr/research/golang-hedp/internal/library"
)

// resolveVersion picks the library version a request renders against.
//
// A request names a revision explicitly, or gets the default. Naming a
// revision that is not resident is a 404 rather than a silent fallback: a
// service whose whole point is "render exactly the charts from commit X" must
// never quietly render commit Y instead.
func resolveVersion(w http.ResponseWriter, r *http.Request, reg *library.Registry) (*library.Version, bool) {
	revision := r.URL.Query().Get("revision")
	if revision == "" {
		v, ok := reg.Default()
		if !ok {
			encode(w, http.StatusServiceUnavailable, errorResponse{
				Error: "no library version is loaded; push one to /v1/versions/{revision}",
			})
			return nil, false
		}
		return v, true
	}

	// Pull it if this replica does not have it. Behind a load balancer that is
	// the normal case, not an error: a version pushed to one pod was never
	// seen by the others, so every pod resolves revisions independently.
	v, err := reg.GetOrFetch(r.Context(), revision)
	if err != nil {
		if errors.Is(err, library.ErrRevisionNotFound) {
			encode(w, http.StatusNotFound, errorResponse{
				Error: fmt.Sprintf("revision %q is not available: %v", revision, err),
			})
			return nil, false
		}
		// The artifact store is unreachable. That is our problem, not the
		// caller's, and it is retryable - so 503, not 404.
		encode(w, http.StatusServiceUnavailable, errorResponse{
			Error: fmt.Sprintf("could not load revision %q: %v", revision, err),
		})
		return nil, false
	}
	return v, true
}

// handleListVersions reports what is resident and what it costs.
func handleListVersions(reg *library.Registry) http.Handler {
	type response struct {
		Default   string          `json:"default"`
		Versions  []library.Stats `json:"versions"`
		UsedBytes int64           `json:"used_bytes"`
		Budget    int64           `json:"budget_bytes"`
		Count     int             `json:"count"`
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		used, budget, count := reg.Usage()
		encode(w, http.StatusOK, response{
			Default:   reg.DefaultRevision(),
			Versions:  reg.Resident(),
			UsedBytes: used,
			Budget:    budget,
			Count:     count,
		})
	})
}

// handlePutVersion accepts a library bundle and makes it servable.
//
// The body is the bundle itself rather than JSON: it is 13 MB of already
// compressed data, and base64-ing it into a JSON field would cost a third more
// bytes and a full copy on both ends for no benefit.
func handlePutVersion(logger *slog.Logger, reg *library.Registry, cfg Config) http.Handler {
	type response struct {
		Revision string        `json:"revision"`
		Loaded   bool          `json:"loaded"`
		Stats    library.Stats `json:"stats"`
		Default  string        `json:"default"`
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		revision := r.PathValue("revision")
		if revision == "" {
			encode(w, http.StatusBadRequest, errorResponse{Error: "revision is required"})
			return
		}

		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, cfg.MaxBundleBytes))
		if err != nil {
			encode(w, http.StatusRequestEntityTooLarge, errorResponse{
				Error: fmt.Sprintf("could not read bundle: %v", err),
			})
			return
		}
		if len(body) == 0 {
			encode(w, http.StatusBadRequest, errorResponse{Error: "bundle body is empty"})
			return
		}

		start := time.Now()
		v, err := reg.LoadBundle(revision, body)
		if err != nil {
			// A bundle that will not fit the memory budget is the operator's
			// problem to size, not a malformed request, but either way the
			// caller needs the reason.
			encode(w, http.StatusUnprocessableEntity, errorResponse{Error: err.Error()})
			return
		}

		logger.InfoContext(r.Context(), "library version loaded",
			"revision", revision,
			"bundle_mb", len(body)>>20,
			"raw_mb", v.Catalog.Stats().TotalBytes>>20,
			"footprint_mb", v.Footprint>>20,
			"elapsed", time.Since(start),
		)

		if q := r.URL.Query().Get("default"); q == "true" {
			if err := reg.SetDefault(revision); err != nil {
				encode(w, http.StatusInternalServerError, errorResponse{Error: err.Error()})
				return
			}
		}

		encode(w, http.StatusOK, response{
			Revision: revision,
			Loaded:   true,
			Stats:    v.Stats(),
			Default:  reg.DefaultRevision(),
		})
	})
}

func handleDeleteVersion(reg *library.Registry) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		revision := r.PathValue("revision")
		if !reg.Evict(revision) {
			encode(w, http.StatusNotFound, errorResponse{Error: "revision " + revision + " is not resident"})
			return
		}
		encode(w, http.StatusOK, map[string]any{
			"revision": revision,
			"evicted":  true,
			"default":  reg.DefaultRevision(),
		})
	})
}

func handleSetDefaultVersion(reg *library.Registry) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		revision := r.PathValue("revision")
		if err := reg.SetDefault(revision); err != nil {
			encode(w, http.StatusNotFound, errorResponse{Error: err.Error()})
			return
		}
		encode(w, http.StatusOK, map[string]any{"default": revision})
	})
}
