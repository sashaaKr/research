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
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/sashaakr/research/golang-hedp/internal/library"
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
		revision   = fs.String("revision", envOr(getenv, "HEDP_REVISION", "local"), "revision name for the library loaded at startup")
		budgetMB   = fs.Int("memory-budget-mb", envOrInt(getenv, "HEDP_MEMORY_BUDGET_MB", 2048), "heap budget across all resident library versions")
		bundleURL  = fs.String("bundle-url", envOr(getenv, "HEDP_BUNDLE_URL", ""), "template for pulling a revision's bundle, must contain {revision}")
		maxFlight  = fs.Int("max-in-flight", envOrInt(getenv, "HEDP_MAX_IN_FLIGHT", 2), "concurrent render requests before shedding with 503 (0 disables)")
		cached     = fs.Bool("cached-engine", envOr(getenv, "HEDP_CACHED_ENGINE", "") == "true", "parse charts once at startup instead of on every render")
		strict     = fs.Bool("strict", envOr(getenv, "HEDP_STRICT", "") == "true", "treat unresolved template keys as errors")
		timeout    = fs.Duration("timeout", 10*time.Minute, "per-request timeout")
	)
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	logger := slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	opts := render.Options{
		MaxConcurrentRenders: *maxRenders,
		ReleaseConcurrency:   *relConc,
		Strict:               *strict,
		CachedEngine:         *cached,
	}
	if *cacheSize > 0 {
		opts.Cache = render.NewCache(*cacheSize)
	}

	// Versions live in RAM, keyed by revision, because a release set
	// corresponds to a commit and a process should be able to serve several at
	// once without a checkout per version.
	//
	// The fetcher is what makes more than one replica work: behind a load
	// balancer a pod cannot be pushed a version, so it pulls the ones it is
	// asked for.
	regCfg := library.Config{
		MemoryBudget:  int64(*budgetMB) << 20,
		RenderOptions: opts,
	}
	if *bundleURL != "" {
		fetcher, err := library.NewHTTPFetcher(*bundleURL, 512<<20)
		if err != nil {
			return err
		}
		regCfg.Fetcher = fetcher
		logger.Info("bundle source configured", "url_template", *bundleURL)
	}
	reg := library.NewRegistry(regCfg)

	// The startup library, if there is one, is loaded from the directory
	// rather than packed into a bundle first: reading files in parallel is
	// several times faster than decompressing the same bytes on one core.
	if *libraryDir != "" {
		v, err := reg.LoadDirectory(*revision, *libraryDir)
		if err != nil {
			return fmt.Errorf("load startup library: %w", err)
		}
		stats := v.Catalog.Stats()
		logger.Info("library version loaded",
			"revision", v.Revision,
			"charts", stats.Charts,
			"releases", len(v.Blueprint.Releases),
			"template_mb", stats.TemplateBytes>>20,
			"files_mb", stats.FileBytes>>20,
			"crd_mb", stats.CRDBytes>>20,
			"total_mb", stats.TotalBytes>>20,
			"footprint_mb", v.Footprint>>20,
			"load_millis", v.LoadMillis,
			"compile_millis", v.Renderer.CompileMillis,
		)
	} else if *bundleURL != "" && *revision != "" {
		// Pull the startup revision before serving. Every replica is given the
		// same -revision, so they all agree on the default; without that, two
		// pods could answer an unqualified request from different commits.
		v, err := reg.GetOrFetch(ctx, *revision)
		if err != nil {
			return fmt.Errorf("pull startup revision %s: %w", *revision, err)
		}
		logger.Info("startup revision pulled",
			"revision", v.Revision,
			"bundle_mb", v.BundleBytes>>20,
			"footprint_mb", v.Footprint>>20,
			"load_millis", v.LoadMillis,
		)
	} else if *bundleURL != "" {
		logger.Warn("no -revision given; this pod has no default and will only serve requests that pin ?revision=")
	} else {
		logger.Warn("started with no library and no -bundle-url; every render will 503 until a version is pushed")
	}

	// Rendering allocates ~67 MB per typical customer, so the Go default of
	// GOGC=100 spends about a quarter of the machine collecting. Measured:
	// GOGC=400 is worth 24% throughput here. Say so rather than overriding it,
	// since the right value depends on the container's memory limit.
	if getenv("GOGC") == "" {
		logger.Info("GOGC is unset; this workload is allocation-heavy and GOGC=400 measured ~24% faster (pair with GOMEMLIMIT)")
	}

	handler := NewServer(logger, reg, Config{
		MaxConfigs:      *maxConfigs,
		BulkConcurrency: *bulkConc,
		RequestTimeout:  *timeout,
		MaxInFlight:     *maxFlight,
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
