package server

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/sashaakr/research/golang-http-service/internal/auth"
	"github.com/sashaakr/research/golang-http-service/internal/logging"
)

// withRequestID is the outermost per-request middleware (apart from
// recover). It attaches a logging bag to the context, generates or
// echoes the X-Request-Id header, and seeds the bag with request_id so
// every downstream log line carries it.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := logging.WithBag(r.Context())

		reqID := r.Header.Get("X-Request-Id")
		if reqID == "" {
			reqID = newRequestID()
		}
		w.Header().Set("X-Request-Id", reqID)
		logging.AddAttrs(ctx, slog.String("request_id", reqID))

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func withRequestLogging(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)
		// LogAttrs(ctx, ...) so ContextHandler can merge in the bag
		// attrs accumulated during the request (request_id, user_id, ...).
		logger.LogAttrs(r.Context(), slog.LevelInfo, "request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", rw.status),
			slog.Duration("duration", time.Since(start)),
		)
	})
}

func withRecover(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				logger.ErrorContext(r.Context(), "panic",
					"err", rec,
					"stack", string(debug.Stack()),
				)
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// newAuthMiddleware returns the article's factory-style middleware:
// dependencies (logger, authenticator) are bound once, and the returned
// func wraps each next handler. On success, the authenticated user is
// attached both to the context (for handlers) and to the logging bag
// (so every subsequent log line — including the request-log line
// emitted by withRequestLogging — carries user_id / admin).
func newAuthMiddleware(logger *slog.Logger, auther auth.Authenticator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := bearerToken(r)
			if !ok {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			user, err := auther.Authenticate(r.Context(), token)
			if err != nil {
				if errors.Is(err, auth.ErrInvalidToken) {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
				logger.ErrorContext(r.Context(), "authenticate", "err", err)
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
			logging.AddAttrs(r.Context(),
				slog.String("user_id", user.ID),
				slog.Bool("admin", user.Admin),
			)
			next.ServeHTTP(w, r.WithContext(auth.WithUser(r.Context(), user)))
		})
	}
}

func adminOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := auth.FromContext(r.Context())
		if !ok || !user.Admin {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func bearerToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	token, ok := strings.CutPrefix(header, "Bearer ")
	if !ok || token == "" {
		return "", false
	}
	return token, true
}

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(b[:])
}
