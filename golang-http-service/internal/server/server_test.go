package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sashaakr/research/golang-http-service/internal/store"
)

const (
	userToken  = "demo-user-token"
	adminToken = "demo-admin-token"
)

// TestRun boots the real binary via Run() on a random port and exercises
// it over HTTP. Each test gets a self-contained instance — context
// cancellation on cleanup gracefully shuts the server down.
func TestRun(t *testing.T) {
	t.Parallel()

	base := startServer(t)
	must := mustFn(t)

	t.Run("healthz is public", func(t *testing.T) {
		resp := must(http.Get(base + "/healthz"))
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
	})

	t.Run("widgets require auth", func(t *testing.T) {
		resp := must(http.Get(base + "/api/widgets"))
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
	})

	t.Run("invalid token is rejected", func(t *testing.T) {
		resp := must(do(t, http.MethodGet, base+"/api/widgets", "garbage", ""))
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
	})

	t.Run("create then get as authenticated user", func(t *testing.T) {
		resp := must(do(t, http.MethodPost, base+"/api/widgets", userToken, `{"name":"sprocket","price":42}`))
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("create status = %d, body = %s", resp.StatusCode, b)
		}
		var created struct {
			Widget store.Widget `json:"widget"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
			t.Fatal(err)
		}
		if created.Widget.ID == "" || created.Widget.Name != "sprocket" {
			t.Fatalf("unexpected: %+v", created.Widget)
		}

		resp2 := must(do(t, http.MethodGet, base+"/api/widgets/"+created.Widget.ID, userToken, ""))
		defer resp2.Body.Close()
		if resp2.StatusCode != http.StatusOK {
			t.Fatalf("get status = %d", resp2.StatusCode)
		}
	})

	t.Run("validation problems", func(t *testing.T) {
		resp := must(do(t, http.MethodPost, base+"/api/widgets", userToken, `{"name":"","price":-1}`))
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422", resp.StatusCode)
		}
		b, _ := io.ReadAll(resp.Body)
		if !strings.Contains(string(b), "name") || !strings.Contains(string(b), "price") {
			t.Fatalf("expected problems for name and price, got: %s", b)
		}
	})

	t.Run("delete is admin-only — user gets 404", func(t *testing.T) {
		// Create a widget to delete (as a user, that's fine).
		c := must(do(t, http.MethodPost, base+"/api/widgets", userToken, `{"name":"to-delete","price":1}`))
		var created struct {
			Widget store.Widget `json:"widget"`
		}
		_ = json.NewDecoder(c.Body).Decode(&created)
		c.Body.Close()

		resp := must(do(t, http.MethodDelete, base+"/api/widgets/"+created.Widget.ID, userToken, ""))
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (admin gate hides the route)", resp.StatusCode)
		}
	})

	t.Run("delete as admin succeeds", func(t *testing.T) {
		c := must(do(t, http.MethodPost, base+"/api/widgets", adminToken, `{"name":"admin-delete","price":1}`))
		var created struct {
			Widget store.Widget `json:"widget"`
		}
		_ = json.NewDecoder(c.Body).Decode(&created)
		c.Body.Close()

		resp := must(do(t, http.MethodDelete, base+"/api/widgets/"+created.Widget.ID, adminToken, ""))
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("delete status = %d, want 204", resp.StatusCode)
		}

		gone := must(do(t, http.MethodGet, base+"/api/widgets/"+created.Widget.ID, adminToken, ""))
		defer gone.Body.Close()
		if gone.StatusCode != http.StatusNotFound {
			t.Fatalf("get-after-delete status = %d, want 404", gone.StatusCode)
		}
	})

	t.Run("missing widget", func(t *testing.T) {
		resp := must(do(t, http.MethodGet, base+"/api/widgets/does-not-exist", userToken, ""))
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
	})

	t.Run("hello template uses sync.Once and is public", func(t *testing.T) {
		resp := must(http.Get(base + "/hello/sasha"))
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		b, _ := io.ReadAll(resp.Body)
		if !strings.Contains(string(b), "Hello, sasha!") {
			t.Fatalf("unexpected body: %s", b)
		}
	})
}

func startServer(t *testing.T) string {
	t.Helper()
	port := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- Run(
			ctx,
			[]string{"test", "-host", "127.0.0.1", "-port", port},
			func(string) string { return "" },
			strings.NewReader(""),
			io.Discard,
			io.Discard,
		)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run returned: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Run did not shut down in time")
		}
	})

	base := "http://127.0.0.1:" + port
	if err := waitForReady(ctx, 5*time.Second, base+"/healthz"); err != nil {
		t.Fatalf("waitForReady: %v", err)
	}
	return base
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return fmt.Sprint(l.Addr().(*net.TCPAddr).Port)
}

func do(t *testing.T, method, url, token, body string) (*http.Response, error) {
	t.Helper()
	var br io.Reader
	if body != "" {
		br = bytes.NewBufferString(body)
	}
	req, err := http.NewRequest(method, url, br)
	if err != nil {
		return nil, err
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return http.DefaultClient.Do(req)
}

func mustFn(t *testing.T) func(*http.Response, error) *http.Response {
	return func(resp *http.Response, err error) *http.Response {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
}

// waitForReady calls endpoint until it returns 200 or until ctx is
// cancelled or timeout elapses. Matches the helper from the article.
func waitForReady(ctx context.Context, timeout time.Duration, endpoint string) error {
	client := http.Client{}
	startTime := time.Now()
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return fmt.Errorf("create request: %w", err)
		}
		resp, err := client.Do(req)
		if err == nil {
			if resp.StatusCode == http.StatusOK {
				resp.Body.Close()
				return nil
			}
			resp.Body.Close()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			if time.Since(startTime) >= timeout {
				return fmt.Errorf("timeout waiting for %s", endpoint)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
}
