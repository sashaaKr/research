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

// TestRun boots the real binary via Run() on a random port and exercises
// it over HTTP. Each test gets a self-contained instance — context
// cancellation on cleanup gracefully shuts the server down.
func TestRun(t *testing.T) {
	t.Parallel()

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

	t.Run("create then get", func(t *testing.T) {
		body := bytes.NewBufferString(`{"name":"sprocket","price":42}`)
		resp, err := http.Post(base+"/api/widgets", "application/json", body)
		if err != nil {
			t.Fatal(err)
		}
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
			t.Fatalf("unexpected created widget: %+v", created.Widget)
		}

		resp2, err := http.Get(base + "/api/widgets/" + created.Widget.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer resp2.Body.Close()
		if resp2.StatusCode != http.StatusOK {
			t.Fatalf("get status = %d", resp2.StatusCode)
		}
		var got store.Widget
		if err := json.NewDecoder(resp2.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		if got != created.Widget {
			t.Fatalf("get != create: %+v vs %+v", got, created.Widget)
		}
	})

	t.Run("validation problems", func(t *testing.T) {
		body := bytes.NewBufferString(`{"name":"","price":-1}`)
		resp, err := http.Post(base+"/api/widgets", "application/json", body)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422", resp.StatusCode)
		}
		b, _ := io.ReadAll(resp.Body)
		if !strings.Contains(string(b), "name") || !strings.Contains(string(b), "price") {
			t.Fatalf("expected problems for name and price, got: %s", b)
		}
	})

	t.Run("missing widget", func(t *testing.T) {
		resp, err := http.Get(base + "/api/widgets/does-not-exist")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
	})

	t.Run("hello template uses sync.Once", func(t *testing.T) {
		resp, err := http.Get(base + "/hello/sasha")
		if err != nil {
			t.Fatal(err)
		}
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

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return fmt.Sprint(l.Addr().(*net.TCPAddr).Port)
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
