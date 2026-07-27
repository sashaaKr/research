package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"time"

	"github.com/sashaakr/research/golang-hedp/internal/catalog"
	"github.com/sashaakr/research/golang-hedp/internal/customer"
	"github.com/sashaakr/research/golang-hedp/internal/render"
)

func handleHealth() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		encode(w, http.StatusOK, map[string]string{"status": "ok"})
	})
}

func handleNotFound() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		encode(w, http.StatusNotFound, errorResponse{Error: "no such route: " + r.Method + " " + r.URL.Path})
	})
}

// handleLibrary reports what was loaded. The byte split is the useful part:
// it is the only place that shows how much of the library the engine will
// actually parse, versus how much is CRDs and files that cost nothing to
// render and everything to hold in memory.
func handleLibrary(cat *catalog.Catalog, renderer *render.Renderer) http.Handler {
	type response struct {
		Catalog  catalog.Stats `json:"catalog"`
		Charts   []string      `json:"charts"`
		Releases []string      `json:"releases"`
		Features []string      `json:"features"`
		MaxDepth int           `json:"max_dependency_depth"`
	}

	// The catalog never changes after startup, so build the payload once.
	bp := renderer.Blueprint()
	features := bp.Features()
	sort.Strings(features)
	full := bp.PlanFor(nil, true)

	resp := response{
		Catalog:  cat.Stats(),
		Charts:   cat.Names(),
		Releases: bp.Names(),
		Features: features,
		MaxDepth: len(full.Waves),
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		encode(w, http.StatusOK, resp)
	})
}

// handlePlan answers "what would you render" without rendering. Planning is
// pure graph work - microseconds against a render's milliseconds - so this is
// the cheap way for a caller to see the blast radius of a feature flag before
// paying for it.
func handlePlan(renderer *render.Renderer, cfg Config) http.Handler {
	type response struct {
		CustomerID string            `json:"customer_id"`
		Problems   map[string]string `json:"problems,omitempty"`
		Selected   []string          `json:"selected,omitempty"`
		PulledIn   []string          `json:"pulled_in,omitempty"`
		Waves      [][]string        `json:"waves,omitempty"`
		Count      int               `json:"count"`
		Width      int               `json:"max_wave_width"`
		Micros     int64             `json:"micros"`
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conf, ok := decode[customer.Config](w, r, cfg.MaxBodyBytes)
		if !ok {
			return
		}
		start := time.Now()
		if problems := conf.Valid(renderer.Features()); len(problems) > 0 {
			encode(w, http.StatusUnprocessableEntity, response{
				CustomerID: conf.ID, Problems: problems, Micros: time.Since(start).Microseconds(),
			})
			return
		}
		plan := renderer.Blueprint().PlanFor(conf.Features, conf.RenderAll)
		encode(w, http.StatusOK, response{
			CustomerID: conf.ID,
			Selected:   plan.Selected,
			PulledIn:   plan.PulledIn,
			Waves:      plan.Waves,
			Count:      plan.Count(),
			Width:      plan.Width(),
			Micros:     time.Since(start).Microseconds(),
		})
	})
}

// handleRender renders one customer configuration.
//
// ?manifests=true returns the rendered YAML. It is off by default because a
// full-library render is ~40 MB of manifests, and the question this service
// answers is usually "does it render", not "show me every byte".
func handleRender(renderer *render.Renderer, cfg Config) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conf, ok := decode[customer.Config](w, r, cfg.MaxBodyBytes)
		if !ok {
			return
		}

		ctx, cancel := contextWithTimeout(r, cfg.RequestTimeout)
		defer cancel()

		res := renderer.Render(ctx, conf)

		// Validation failures are the client's fault and are reported as 422
		// with the field-level problems; a template that blew up is the
		// platform's fault and is a 200 with per-release errors, because in a
		// bulk world the caller still needs the results for everything else.
		if len(res.Problems) > 0 {
			encode(w, http.StatusUnprocessableEntity, res)
			return
		}
		if wantManifests(r) {
			// Re-render with payloads attached. Keeping this off the default
			// path means the shared Renderer never allocates manifests that
			// will only be thrown away.
			res = renderer.Verbose().Render(ctx, conf)
		}
		encode(w, http.StatusOK, res)
	})
}

func wantManifests(r *http.Request) bool {
	v, _ := strconv.ParseBool(r.URL.Query().Get("manifests"))
	return v
}

// handleRenderBulk is the endpoint the whole design exists for.
//
// The response is newline-delimited JSON, streamed as each customer finishes,
// followed by a final summary line. Streaming is not a nicety: with thousands
// of configs, buffering a single JSON array means holding every result in
// memory and sending nothing until the slowest customer completes.
func handleRenderBulk(logger *slog.Logger, renderer *render.Renderer, cfg Config) http.Handler {
	type request struct {
		Configs     []customer.Config `json:"configs"`
		Concurrency int               `json:"concurrency,omitempty"`
	}
	type envelope struct {
		Type    string              `json:"type"` // "result" | "summary"
		Result  *render.Result      `json:"result,omitempty"`
		Summary *render.BulkSummary `json:"summary,omitempty"`
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req, ok := decode[request](w, r, cfg.MaxBodyBytes)
		if !ok {
			return
		}
		if len(req.Configs) == 0 {
			encode(w, http.StatusBadRequest, errorResponse{Error: "configs must not be empty"})
			return
		}
		if len(req.Configs) > cfg.MaxConfigs {
			encode(w, http.StatusRequestEntityTooLarge, errorResponse{
				Error: fmt.Sprintf("%d configs exceeds the limit of %d", len(req.Configs), cfg.MaxConfigs),
			})
			return
		}

		concurrency := req.Concurrency
		if concurrency <= 0 {
			concurrency = cfg.BulkConcurrency
		}
		if concurrency <= 0 {
			concurrency = runtime.GOMAXPROCS(0)
		}

		ctx, cancel := contextWithTimeout(r, cfg.RequestTimeout)
		defer cancel()

		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)

		bw := bufio.NewWriterSize(w, 64<<10)
		enc := json.NewEncoder(bw)
		flusher, _ := w.(http.Flusher)

		start := time.Now()
		var written int

		summary := renderer.RenderBulk(ctx, req.Configs, concurrency, func(res render.Result) error {
			r := res
			if err := enc.Encode(envelope{Type: "result", Result: &r}); err != nil {
				return err
			}
			written++
			// Flush periodically rather than per result: a flush per customer
			// on a 4000-config batch is 4000 syscalls of pure overhead, but
			// never flushing means the client sees nothing until the end.
			if written%64 == 0 {
				if err := bw.Flush(); err != nil {
					return err
				}
				if flusher != nil {
					flusher.Flush()
				}
			}
			return nil
		})

		if err := enc.Encode(envelope{Type: "summary", Summary: &summary}); err != nil {
			logger.WarnContext(ctx, "bulk summary write failed", "error", err)
		}
		if err := bw.Flush(); err != nil {
			logger.WarnContext(ctx, "bulk flush failed", "error", err)
		}

		logger.InfoContext(ctx, "bulk render complete",
			"configs", summary.Configs,
			"ok", summary.OK,
			"invalid", summary.Invalid,
			"failed", summary.Failed,
			"releases", summary.Releases,
			"concurrency", summary.Concurrency,
			"mb", summary.TotalBytes>>20,
			"elapsed", time.Since(start),
		)
	})
}
