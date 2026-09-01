package render

import (
	"context"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/sashaakr/research/golang-hedp/internal/customer"
)

// BulkSummary aggregates a bulk run. The percentiles are per-customer wall
// time under load, which is the number that matters for a bulk API: the mean
// hides the customers who happened to select the two enormous charts.
type BulkSummary struct {
	Configs   int `json:"configs"`
	OK        int `json:"ok"`
	Invalid   int `json:"invalid"`
	Failed    int `json:"failed"`
	Releases  int `json:"releases"`
	Manifests int `json:"manifests"`

	TotalBytes int64 `json:"total_bytes"`
	Millis     int64 `json:"millis"`

	ConfigsPerSecond  float64 `json:"configs_per_second"`
	ReleasesPerSecond float64 `json:"releases_per_second"`
	MBPerSecond       float64 `json:"mb_per_second"`

	P50Micros int64 `json:"p50_micros"`
	P95Micros int64 `json:"p95_micros"`
	P99Micros int64 `json:"p99_micros"`
	MaxMicros int64 `json:"max_micros"`

	Concurrency int         `json:"concurrency"`
	Cache       *CacheStats `json:"cache,omitempty"`

	// Truncated is set when the context was cancelled before every config was
	// processed - a client disconnect on a 4000-config request should not look
	// like a clean result.
	Truncated bool `json:"truncated,omitempty"`
}

// RenderBulk renders many configurations, calling emit once per result as it
// completes.
//
// Results stream rather than accumulate. A bulk request of a few thousand
// configs against a 55 MB library produces gigabytes of manifests; buffering
// them to build one response body is the difference between a service that
// holds a steady RSS and one that is killed by the OOM reaper. emit is called
// from a single goroutine, so implementations need no locking of their own.
func (r *Renderer) RenderBulk(ctx context.Context, configs []customer.Config, concurrency int, emit func(Result) error) BulkSummary {
	start := time.Now()

	if concurrency <= 0 {
		concurrency = runtime.GOMAXPROCS(0)
	}
	if concurrency > len(configs) {
		concurrency = len(configs)
	}
	summary := BulkSummary{Configs: len(configs), Concurrency: concurrency}
	if len(configs) == 0 {
		return summary
	}

	jobs := make(chan customer.Config)
	out := make(chan Result, concurrency)

	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for cfg := range jobs {
				select {
				case out <- r.Render(ctx, cfg):
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	go func() {
		defer close(jobs)
		for _, cfg := range configs {
			select {
			case jobs <- cfg:
			case <-ctx.Done():
				return
			}
		}
	}()

	go func() {
		wg.Wait()
		close(out)
	}()

	latencies := make([]int64, 0, len(configs))
	var emitted int
	for res := range out {
		emitted++
		latencies = append(latencies, res.Micros)
		summary.Releases += len(res.Releases)
		summary.Manifests += res.Manifests
		summary.TotalBytes += int64(res.TotalBytes)
		switch {
		case len(res.Problems) > 0:
			summary.Invalid++
		case res.OK:
			summary.OK++
		default:
			summary.Failed++
		}
		if emit != nil {
			if err := emit(res); err != nil {
				// The client went away. Stop early rather than rendering
				// thousands more configs nobody will read.
				break
			}
		}
	}

	elapsed := time.Since(start)
	summary.Millis = elapsed.Milliseconds()
	summary.Truncated = emitted < len(configs)

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	summary.P50Micros = percentile(latencies, 0.50)
	summary.P95Micros = percentile(latencies, 0.95)
	summary.P99Micros = percentile(latencies, 0.99)
	if n := len(latencies); n > 0 {
		summary.MaxMicros = latencies[n-1]
	}

	if secs := elapsed.Seconds(); secs > 0 {
		summary.ConfigsPerSecond = float64(emitted) / secs
		summary.ReleasesPerSecond = float64(summary.Releases) / secs
		summary.MBPerSecond = float64(summary.TotalBytes) / (1 << 20) / secs
	}
	if r.opts.Cache != nil {
		st := r.opts.Cache.Stats()
		summary.Cache = &st
	}
	return summary
}

func percentile(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(p * float64(len(sorted)))
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
