// Package server exposes the renderer over HTTP.
package server

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/sashaakr/research/golang-hedp/internal/library"
)

// Config holds the server's request-shaping limits. They exist because this is
// a bulk API: without a cap, one request can ask for more work than the
// process can survive.
type Config struct {
	// MaxBodyBytes caps the decoded request body.
	MaxBodyBytes int64
	// MaxConfigs caps how many customer configs one bulk request may carry.
	MaxConfigs int
	// BulkConcurrency is the default number of customers rendered in parallel.
	BulkConcurrency int
	// RequestTimeout bounds a single request. A full-library render for
	// thousands of customers is minutes of CPU, so the default is generous.
	RequestTimeout time.Duration
	// MaxBundleBytes caps an uploaded library version.
	MaxBundleBytes int64
}

func (c Config) withDefaults() Config {
	if c.MaxBodyBytes <= 0 {
		c.MaxBodyBytes = 64 << 20
	}
	if c.MaxConfigs <= 0 {
		c.MaxConfigs = 10000
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = 10 * time.Minute
	}
	if c.MaxBundleBytes <= 0 {
		c.MaxBundleBytes = 512 << 20
	}
	return c
}

// NewServer wires the routes and middleware and returns the whole service as
// one http.Handler.
func NewServer(logger *slog.Logger, reg *library.Registry, cfg Config) http.Handler {
	cfg = cfg.withDefaults()

	mux := http.NewServeMux()
	addRoutes(mux, logger, reg, cfg)

	var handler http.Handler = mux
	handler = requestLogger(logger, handler)
	handler = recoverPanics(logger, handler)
	return handler
}
