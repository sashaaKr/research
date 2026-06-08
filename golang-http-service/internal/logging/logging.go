// Package logging provides per-request structured-logging context for
// the HTTP service. The ContextHandler wraps any slog.Handler and copies
// attributes from a per-request bag onto every record, so call sites
// just need to use the *Context variants (logger.InfoContext, etc.) or
// the package-level slog.InfoContext.
//
// Pattern:
//
//  1. Outermost middleware calls WithBag(ctx) once at the start of the
//     request, attaching an empty bag.
//  2. Any middleware or handler can call AddAttrs(ctx, ...) to enrich
//     the bag (request_id, user_id, trace_id, ...).
//  3. Any log made via logger.InfoContext(ctx, ...) or
//     slog.InfoContext(ctx, ...) automatically picks up the current
//     contents of the bag, including attributes added later in the
//     request than where the log call was originally registered (e.g.
//     a deferred request-log line will include auth attrs added by an
//     inner middleware).
package logging

import (
	"context"
	"io"
	"log/slog"
	"sync"
)

type bag struct {
	mu    sync.Mutex
	attrs []slog.Attr
}

type ctxKey struct{}

// WithBag returns a context carrying an empty attribute bag. Should be
// called once per request, near the entry point.
func WithBag(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxKey{}, &bag{})
}

// AddAttrs appends slog attributes to the per-request bag. No-op if the
// context has no bag (so code outside the HTTP path stays safe to call).
func AddAttrs(ctx context.Context, attrs ...slog.Attr) {
	b, ok := ctx.Value(ctxKey{}).(*bag)
	if !ok {
		return
	}
	b.mu.Lock()
	b.attrs = append(b.attrs, attrs...)
	b.mu.Unlock()
}

// ContextHandler wraps a slog.Handler and merges per-request bag
// attributes into every record. WithAttrs and WithGroup re-wrap to
// preserve the behaviour through child loggers created via logger.With.
type ContextHandler struct {
	slog.Handler
}

func (h ContextHandler) Handle(ctx context.Context, r slog.Record) error {
	if b, ok := ctx.Value(ctxKey{}).(*bag); ok {
		b.mu.Lock()
		// Copy so we don't hold the lock while the underlying handler
		// formats and writes the record.
		copied := append([]slog.Attr(nil), b.attrs...)
		b.mu.Unlock()
		r.AddAttrs(copied...)
	}
	return h.Handler.Handle(ctx, r)
}

func (h ContextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return ContextHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h ContextHandler) WithGroup(name string) slog.Handler {
	return ContextHandler{Handler: h.Handler.WithGroup(name)}
}

// NewLogger builds a slog.Logger writing text to w, wrapped in
// ContextHandler so per-request bag attributes are automatically
// included on every record.
func NewLogger(w io.Writer, level slog.Level) *slog.Logger {
	base := slog.NewTextHandler(w, &slog.HandlerOptions{Level: level})
	return slog.New(ContextHandler{Handler: base})
}
