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

// TestRun spins the real binary up via Run() on a random port and exercises
// the HTTP surface — the pattern the article recommends so tests touch the
// same wiring main uses.
func TestRun(t *testing.T) {
	port := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- Run(
			ctx,
			[]string{"test", "-host", "127.0.0.1", "-port", port},
			func(string) string { return "" },
			io.Discard,
			io.Discard,
		)
	}()

	base := "http://127.0.0.1:" + port
	waitForReady(t, base+"/healthz")

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

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not shut down in time")
	}
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

func waitForReady(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	client := &http.Client{Timeout: 200 * time.Millisecond}
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("server did not become ready at %s", url)
}
