package server

import (
	"context"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"sync"

	"github.com/sashaakr/research/golang-http-service/internal/store"
)

func handleHealthz() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = encode(w, r, http.StatusOK, map[string]string{"status": "ok"})
	})
}

func handleListWidgets(logger *slog.Logger, widgets store.Store) http.Handler {
	type response struct {
		Widgets []store.Widget `json:"widgets"`
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		items, err := widgets.List(r.Context())
		if err != nil {
			logger.ErrorContext(r.Context(), "list widgets", "err", err)
			http.Error(w, "list failed", http.StatusInternalServerError)
			return
		}
		_ = encode(w, r, http.StatusOK, response{Widgets: items})
	})
}

func handleGetWidget(logger *slog.Logger, widgets store.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		widget, err := widgets.Get(r.Context(), id)
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "widget not found", http.StatusNotFound)
			return
		}
		if err != nil {
			logger.ErrorContext(r.Context(), "get widget", "id", id, "err", err)
			http.Error(w, "get failed", http.StatusInternalServerError)
			return
		}
		_ = encode(w, r, http.StatusOK, widget)
	})
}

type createWidgetRequest struct {
	Name  string `json:"name"`
	Price int    `json:"price"`
}

func (req createWidgetRequest) Valid(_ context.Context) map[string]string {
	problems := map[string]string{}
	if req.Name == "" {
		problems["name"] = "must not be empty"
	}
	if req.Price < 0 {
		problems["price"] = "must be non-negative"
	}
	return problems
}

func handleCreateWidget(logger *slog.Logger, widgets store.Store) http.Handler {
	type response struct {
		Widget   store.Widget      `json:"widget,omitempty"`
		Problems map[string]string `json:"problems,omitempty"`
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req, problems, err := decodeValid[createWidgetRequest](r)
		if len(problems) > 0 {
			_ = encode(w, r, http.StatusUnprocessableEntity, response{Problems: problems})
			return
		}
		if err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		created, err := widgets.Create(r.Context(), store.Widget{Name: req.Name, Price: req.Price})
		if err != nil {
			logger.ErrorContext(r.Context(), "create widget", "err", err)
			http.Error(w, "create failed", http.StatusInternalServerError)
			return
		}
		_ = encode(w, r, http.StatusCreated, response{Widget: created})
	})
}

// handleHello demonstrates the sync.Once pattern: expensive per-handler
// setup (template parsing) is deferred until the first request, then
// reused. If startup fails, the error is captured and surfaced on every
// subsequent call rather than swallowed.
func handleHello() http.Handler {
	const tmpl = `<!doctype html><html><body><h1>Hello, {{.Name}}!</h1></body></html>`
	var (
		initOnce sync.Once
		tpl      *template.Template
		tplErr   error
	)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		initOnce.Do(func() {
			tpl, tplErr = template.New("hello").Parse(tmpl)
		})
		if tplErr != nil {
			http.Error(w, tplErr.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = tpl.Execute(w, struct{ Name string }{Name: r.PathValue("name")})
	})
}

func handleDeleteWidget(logger *slog.Logger, widgets store.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		err := widgets.Delete(r.Context(), id)
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "widget not found", http.StatusNotFound)
			return
		}
		if err != nil {
			logger.ErrorContext(r.Context(), "delete widget", "id", id, "err", err)
			http.Error(w, "delete failed", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
