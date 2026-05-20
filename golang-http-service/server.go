package main

import (
	"log/slog"
	"net/http"
)

type Config struct {
	Host string
	Port string
}

// NewServer wires up every dependency and returns a single http.Handler.
// Keeping this as a thin constructor — middleware lives here, routes live
// in routes.go — makes the surface easy to test end-to-end via run().
func NewServer(
	logger *slog.Logger,
	cfg Config,
	store Store,
) http.Handler {
	mux := http.NewServeMux()
	addRoutes(mux, logger, cfg, store)

	var handler http.Handler = mux
	handler = withRequestLogging(logger, handler)
	handler = withRecover(logger, handler)
	return handler
}
