package server

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"time"

	"github.com/sashaakr/research/golang-http-service/internal/store"
)

func Run(
	ctx context.Context,
	args []string,
	getenv func(string) string,
	stdin io.Reader,
	stdout io.Writer,
	stderr io.Writer,
) error {
	_ = stdin // reserved for future use (matches article signature)

	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt)
	defer cancel()

	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	host := fs.String("host", firstNonEmpty(getenv("HOST"), "127.0.0.1"), "listen host")
	port := fs.String("port", firstNonEmpty(getenv("PORT"), "8080"), "listen port")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	logger := slog.New(slog.NewTextHandler(stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg := Config{
		Host: *host,
		Port: *port,
	}
	widgets := store.NewMemoryStore()

	srv := NewServer(logger, cfg, widgets)
	httpServer := &http.Server{
		Addr:              net.JoinHostPort(cfg.Host, cfg.Port),
		Handler:           srv,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		logger.Info("listening", "addr", httpServer.Addr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(stderr, "listen: %s\n", err)
		}
	}()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			fmt.Fprintf(stderr, "shutdown: %s\n", err)
		}
	}()
	wg.Wait()
	return nil
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}
