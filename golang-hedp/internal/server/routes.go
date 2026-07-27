package server

import (
	"log/slog"
	"net/http"

	"github.com/sashaakr/research/golang-hedp/internal/catalog"
	"github.com/sashaakr/research/golang-hedp/internal/render"
)

func addRoutes(mux *http.ServeMux, logger *slog.Logger, cat *catalog.Catalog, renderer *render.Renderer, cfg Config) {
	mux.Handle("GET /healthz", handleHealth())
	mux.Handle("GET /v1/library", handleLibrary(cat, renderer))
	mux.Handle("POST /v1/plan", handlePlan(renderer, cfg))
	mux.Handle("POST /v1/render", handleRender(renderer, cfg))
	mux.Handle("POST /v1/render/bulk", handleRenderBulk(logger, renderer, cfg))
	mux.Handle("/", handleNotFound())
}
