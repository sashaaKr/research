package server

import (
	"log/slog"
	"net/http"

	"github.com/sashaakr/research/golang-http-service/internal/auth"
	"github.com/sashaakr/research/golang-http-service/internal/store"
)

func addRoutes(
	mux *http.ServeMux,
	logger *slog.Logger,
	cfg Config,
	widgets store.Store,
	auther auth.Authenticator,
) {
	requireAuth := newAuthMiddleware(logger, auther)

	// public
	mux.Handle("GET /healthz", handleHealthz())
	mux.Handle("GET /hello/{name}", handleHello())

	// authenticated
	mux.Handle("GET /api/widgets", requireAuth(handleListWidgets(logger, widgets)))
	mux.Handle("GET /api/widgets/{id}", requireAuth(handleGetWidget(logger, widgets)))
	mux.Handle("POST /api/widgets", requireAuth(handleCreateWidget(logger, widgets)))

	// admin only
	mux.Handle("DELETE /api/widgets/{id}", requireAuth(adminOnly(handleDeleteWidget(logger, widgets))))

	mux.Handle("/", http.NotFoundHandler())
}
