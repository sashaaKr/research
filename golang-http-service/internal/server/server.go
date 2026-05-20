package server

import (
	"log/slog"
	"net/http"

	"github.com/sashaakr/research/golang-http-service/internal/store"
)

type Config struct {
	Host string
	Port string
}

// NewServer wires up every dependency and returns a single http.Handler.
// Routes live in routes.go; cross-cutting middleware is applied here.
func NewServer(
	logger *slog.Logger,
	cfg Config,
	widgets store.Store,
) http.Handler {
	mux := http.NewServeMux()
	addRoutes(mux, logger, cfg, widgets)

	var handler http.Handler = mux
	handler = withRequestLogging(logger, handler)
	handler = withRecover(logger, handler)
	return handler
}
