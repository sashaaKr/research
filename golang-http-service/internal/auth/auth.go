// Package auth defines the authentication contract used by the HTTP
// middleware in internal/server. Handlers and middleware depend on the
// Authenticator interface; concrete implementations (static, OIDC, JWT,
// database-backed) can slot in without touching HTTP code.
package auth

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// ErrInvalidToken means the token is well-formed but unknown or expired.
// Lookup failures (network, database) should be returned as other errors;
// middleware uses errors.Is(err, ErrInvalidToken) to decide between 401
// and 500.
var ErrInvalidToken = errors.New("invalid token")

type User struct {
	ID    string
	Name  string
	Admin bool
}

type Authenticator interface {
	Authenticate(ctx context.Context, token string) (User, error)
}

type ctxKey struct{}

func WithUser(ctx context.Context, u User) context.Context {
	return context.WithValue(ctx, ctxKey{}, u)
}

func FromContext(ctx context.Context) (User, bool) {
	u, ok := ctx.Value(ctxKey{}).(User)
	return u, ok
}

// StaticAuthenticator resolves a fixed map of token -> User. Useful as a
// demo backend and as a stand-in in tests.
type StaticAuthenticator struct {
	mu     sync.RWMutex
	tokens map[string]User
}

func NewStatic(tokens map[string]User) *StaticAuthenticator {
	cp := make(map[string]User, len(tokens))
	for k, v := range tokens {
		cp[k] = v
	}
	return &StaticAuthenticator{tokens: cp}
}

func (s *StaticAuthenticator) Authenticate(_ context.Context, token string) (User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.tokens[token]
	if !ok {
		return User{}, fmt.Errorf("%w: %q", ErrInvalidToken, token)
	}
	return u, nil
}
