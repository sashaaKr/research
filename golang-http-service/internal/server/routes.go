package server

import (
	"log/slog"
	"net/http"

	"github.com/sashaakr/research/golang-http-service/internal/store"
)

func addRoutes(
	mux *http.ServeMux,
	logger *slog.Logger,
	cfg Config,
	widgets store.Store,
) {
	mux.Handle("GET /healthz", handleHealthz())
	mux.Handle("GET /api/widgets", handleListWidgets(logger, widgets))
	mux.Handle("GET /api/widgets/{id}", handleGetWidget(logger, widgets))
	mux.Handle("POST /api/widgets", handleCreateWidget(logger, widgets))
	mux.Handle("/", http.NotFoundHandler())
}
