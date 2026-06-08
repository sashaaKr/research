package server

import (
	"log/slog"
	"net/http"

	"github.com/sashaakr/research/golang-http-service/internal/auth"
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
	auther auth.Authenticator,
) http.Handler {
	mux := http.NewServeMux()
	addRoutes(mux, logger, cfg, widgets, auther)

	// Outermost first (executed first on the way in). Order matters:
	// withRequestID sets up the logging bag before withRequestLogging
	// emits its deferred request log line, and before any handler runs.
	var handler http.Handler = mux
	handler = withRequestLogging(logger, handler)
	handler = withRequestID(handler)
	handler = withRecover(logger, handler)
	return handler
}
