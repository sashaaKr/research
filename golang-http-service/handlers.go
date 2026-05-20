package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
)

func handleHealthz() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = encode(w, http.StatusOK, map[string]string{"status": "ok"})
	})
}

func handleListWidgets(logger *slog.Logger, store Store) http.Handler {
	type response struct {
		Widgets []Widget `json:"widgets"`
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		widgets, err := store.List(r.Context())
		if err != nil {
			logger.Error("list widgets", "err", err)
			http.Error(w, "list failed", http.StatusInternalServerError)
			return
		}
		_ = encode(w, http.StatusOK, response{Widgets: widgets})
	})
}

func handleGetWidget(logger *slog.Logger, store Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		widget, err := store.Get(r.Context(), id)
		if errors.Is(err, ErrNotFound) {
			http.Error(w, "widget not found", http.StatusNotFound)
			return
		}
		if err != nil {
			logger.Error("get widget", "id", id, "err", err)
			http.Error(w, "get failed", http.StatusInternalServerError)
			return
		}
		_ = encode(w, http.StatusOK, widget)
	})
}

type createWidgetRequest struct {
	Name  string `json:"name"`
	Price int    `json:"price"`
}

func (r createWidgetRequest) Valid(_ context.Context) map[string]string {
	problems := map[string]string{}
	if r.Name == "" {
		problems["name"] = "must not be empty"
	}
	if r.Price < 0 {
		problems["price"] = "must be non-negative"
	}
	return problems
}

func handleCreateWidget(logger *slog.Logger, store Store) http.Handler {
	type response struct {
		Widget   Widget            `json:"widget,omitempty"`
		Problems map[string]string `json:"problems,omitempty"`
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req, problems, err := decode[createWidgetRequest](r)
		if len(problems) > 0 {
			_ = encode(w, http.StatusUnprocessableEntity, response{Problems: problems})
			return
		}
		if err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		created, err := store.Create(r.Context(), Widget{Name: req.Name, Price: req.Price})
		if err != nil {
			logger.Error("create widget", "err", err)
			http.Error(w, "create failed", http.StatusInternalServerError)
			return
		}
		_ = encode(w, http.StatusCreated, response{Widget: created})
	})
}
