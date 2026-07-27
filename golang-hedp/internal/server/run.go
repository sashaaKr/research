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
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/sashaakr/research/golang-hedp/internal/blueprint"
	"github.com/sashaakr/research/golang-hedp/internal/catalog"
	"github.com/sashaakr/research/golang-hedp/internal/render"
)

// Run is the whole program. It takes the OS fundamentals as arguments so a
// test can drive the real entry point with controlled input.
func Run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	ctx, cancel := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		addr       = fs.String("addr", envOr(getenv, "HEDP_ADDR", ":8080"), "listen address")
		libraryDir = fs.String("library", envOr(getenv, "HEDP_LIBRARY", "testdata/library"), "directory holding charts/ and blueprint.yaml")
		maxRenders = fs.Int("max-renders", envOrInt(getenv, "HEDP_MAX_RENDERS", runtime.GOMAXPROCS(0)), "max concurrent chart renders across the process")
		relConc    = fs.Int("release-concurrency", envOrInt(getenv, "HEDP_RELEASE_CONCURRENCY", 1), "parallel releases within one customer (1 = serial)")
		bulkConc   = fs.Int("bulk-concurrency", envOrInt(getenv, "HEDP_BULK_CONCURRENCY", runtime.GOMAXPROCS(0)), "default customers rendered in parallel")
		maxConfigs = fs.Int("max-configs", envOrInt(getenv, "HEDP_MAX_CONFIGS", 10000), "max customer configs per bulk request")
		cacheSize  = fs.Int("cache", envOrInt(getenv, "HEDP_CACHE", 0), "render verdict cache size in entries (0 disables)")
		cached     = fs.Bool("cached-engine", envOr(getenv, "HEDP_CACHED_ENGINE", "") == "true", "parse charts once at startup instead of on every render")
		strict     = fs.Bool("strict", envOr(getenv, "HEDP_STRICT", "") == "true", "treat unresolved template keys as errors")
		timeout    = fs.Duration("timeout", 10*time.Minute, "per-request timeout")
	)
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	logger := slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cat, err := catalog.Load(filepath.Join(*libraryDir, "charts"))
	if err != nil {
		return fmt.Errorf("load catalog: %w", err)
	}
	bp, err := blueprint.Load(filepath.Join(*libraryDir, "blueprint.yaml"))
	if err != nil {
		return fmt.Errorf("load blueprint: %w", err)
	}

	stats := cat.Stats()
	logger.Info("chart library loaded",
		"charts", stats.Charts,
		"releases", len(bp.Releases),
		"template_mb", stats.TemplateBytes>>20,
		"files_mb", stats.FileBytes>>20,
		"crd_mb", stats.CRDBytes>>20,
		"total_mb", stats.TotalBytes>>20,
		"load", stats.LoadDuration,
	)

	opts := render.Options{
		MaxConcurrentRenders: *maxRenders,
		ReleaseConcurrency:   *relConc,
		Strict:               *strict,
		CachedEngine:         *cached,
	}
	if *cacheSize > 0 {
		opts.Cache = render.NewCache(*cacheSize)
	}
	renderer, err := render.New(cat, bp, opts)
	if err != nil {
		return fmt.Errorf("build renderer: %w", err)
	}

	if *cached {
		logger.Info("charts precompiled", "millis", renderer.CompileMillis)
	}

	handler := NewServer(logger, cat, renderer, Config{
		MaxConfigs:      *maxConfigs,
		BulkConcurrency: *bulkConc,
		RequestTimeout:  *timeout,
	})

	srv := &http.Server{
		Addr:              *addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: a bulk render legitimately streams for minutes, and
		// a write deadline would truncate it mid-batch. RequestTimeout bounds
		// the work instead.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}

	errCh := make(chan error, 1)
	go func() {
		fmt.Fprintf(stdout, "hedp listening on %s\n", *addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	fmt.Fprintln(stdout, "hedp stopped")
	return nil
}

func envOr(getenv func(string) string, key, fallback string) string {
	if v := getenv(key); v != "" {
		return v
	}
	return fallback
}

func envOrInt(getenv func(string) string, key string, fallback int) int {
	v := getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}
