package store

import (
	"context"
	"errors"
	"sync"
)

var ErrNotFound = errors.New("not found")

type Widget struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Price int    `json:"price"`
}

type Store interface {
	List(ctx context.Context) ([]Widget, error)
	Get(ctx context.Context, id string) (Widget, error)
	Create(ctx context.Context, w Widget) (Widget, error)
}

type MemoryStore struct {
	mu      sync.RWMutex
	widgets map[string]Widget
	nextID  int
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{widgets: make(map[string]Widget)}
}

func (s *MemoryStore) List(_ context.Context) ([]Widget, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Widget, 0, len(s.widgets))
	for _, w := range s.widgets {
		out = append(out, w)
	}
	return out, nil
}

func (s *MemoryStore) Get(_ context.Context, id string) (Widget, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	w, ok := s.widgets[id]
	if !ok {
		return Widget{}, ErrNotFound
	}
	return w, nil
}

func (s *MemoryStore) Create(_ context.Context, w Widget) (Widget, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	w.ID = idFromInt(s.nextID)
	s.widgets[w.ID] = w
	return w, nil
}

func idFromInt(n int) string {
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = digits[n%10]
		n /= 10
	}
	return string(b[i:])
}
