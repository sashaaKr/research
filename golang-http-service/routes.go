package main

import (
	"log/slog"
	"net/http"
)

func addRoutes(
	mux *http.ServeMux,
	logger *slog.Logger,
	cfg Config,
	store Store,
) {
	mux.Handle("GET /healthz", handleHealthz())
	mux.Handle("GET /api/widgets", handleListWidgets(logger, store))
	mux.Handle("GET /api/widgets/{id}", handleGetWidget(logger, store))
	mux.Handle("POST /api/widgets", handleCreateWidget(logger, store))
	mux.Handle("/", http.NotFoundHandler())
}
