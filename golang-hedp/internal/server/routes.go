package server

import (
	"log/slog"
	"net/http"

	"github.com/sashaakr/research/golang-hedp/internal/library"
)

func addRoutes(mux *http.ServeMux, logger *slog.Logger, reg *library.Registry, cfg Config) {
	mux.Handle("GET /healthz", handleHealth())

	// Rendering. Every one of these takes ?revision=<sha>; without it they use
	// the registry's default.
	mux.Handle("GET /v1/library", handleLibrary(reg))
	mux.Handle("POST /v1/plan", handlePlan(reg, cfg))
	mux.Handle("POST /v1/render", handleRender(reg, cfg))
	mux.Handle("POST /v1/render/bulk", handleRenderBulk(logger, reg, cfg))

	// Version management. A release set corresponds to a commit, so revisions
	// are pushed in as bundles and held in RAM rather than checked out.
	mux.Handle("GET /v1/versions", handleListVersions(reg))
	mux.Handle("PUT /v1/versions/{revision}", handlePutVersion(logger, reg, cfg))
	mux.Handle("DELETE /v1/versions/{revision}", handleDeleteVersion(reg))
	mux.Handle("POST /v1/versions/{revision}/default", handleSetDefaultVersion(reg))

	mux.Handle("/", handleNotFound())
}
