// Command loadgen drives the HEDP bulk API and reports what a client actually
// sees.
//
// The Go benchmarks measure the renderer. This measures the service: JSON
// decode of a large request body, the render, NDJSON encode, and the network
// in between. The gap between the two is the cost of being an HTTP service
// rather than a library, and it is worth knowing before blaming the engine.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/sashaakr/research/golang-hedp/internal/library"
)

func main() {
	if err := run(context.Background(), os.Args, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "loadgen:", err)
		os.Exit(1)
	}
}

type config struct {
	ID        string   `json:"id"`
	Tier      string   `json:"tier"`
	Region    string   `json:"region"`
	Features  []string `json:"features,omitempty"`
	Replicas  int      `json:"replicas,omitempty"`
	RenderAll bool     `json:"render_all,omitempty"`
}

type envelope struct {
	Type    string          `json:"type"`
	Result  json.RawMessage `json:"result,omitempty"`
	Summary json.RawMessage `json:"summary,omitempty"`
}

type result struct {
	CustomerID string `json:"customer_id"`
	OK         bool   `json:"ok"`
	Planned    int    `json:"planned"`
	Manifests  int    `json:"manifests"`
	TotalBytes int    `json:"total_bytes"`
	Micros     int64  `json:"micros"`
}

func run(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	var (
		addr        = fs.String("addr", "http://127.0.0.1:8080", "base URL of the hedp service")
		count       = fs.Int("configs", 200, "number of customer configs per request")
		concurrency = fs.Int("concurrency", 0, "server-side render concurrency (0 = server default)")
		rounds      = fs.Int("rounds", 1, "how many times to send the request")
		renderAll   = fs.Bool("all", false, "make every config render the entire library")
		revision    = fs.String("revision", "", "render against this library revision (default: the server's default)")
		pushDir     = fs.String("push", "", "pack this library directory and PUT it as -revision, then exit")
		chunk       = fs.Int("chunk", 0, "split -configs into requests of this size (0 = one request)")
		clients     = fs.Int("clients", 1, "send chunks from this many concurrent clients")
	)
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	client := &http.Client{Timeout: 30 * time.Minute}

	if *pushDir != "" {
		if *revision == "" {
			return fmt.Errorf("-push needs -revision to name the version")
		}
		return push(ctx, client, *addr, *revision, *pushDir, stdout)
	}

	url := *addr + "/v1/render/bulk"
	if *revision != "" {
		url += "?revision=" + *revision
	}

	all := buildConfigs(*count, *renderAll)
	size := *chunk
	if size <= 0 || size > len(all) {
		size = len(all)
	}

	var bodies [][]byte
	for start := 0; start < len(all); start += size {
		end := min(start+size, len(all))
		b, err := json.Marshal(map[string]any{
			"configs":     all[start:end],
			"concurrency": *concurrency,
		})
		if err != nil {
			return err
		}
		bodies = append(bodies, b)
	}
	fmt.Fprintf(stdout, "%d configs in %d chunk(s) of %d, %d concurrent client(s)\n",
		len(all), len(bodies), size, *clients)

	for round := 1; round <= *rounds; round++ {
		if err := oneRound(ctx, client, url, bodies, *clients, round, stdout); err != nil {
			return err
		}
	}
	return nil
}

// push packs a library directory and uploads it as a revision. It uses the
// packed layout - one gzip stream per chart - because that is what the server
// can decompress in parallel.
func push(ctx context.Context, client *http.Client, addr, revision, dir string, stdout io.Writer) error {
	start := time.Now()
	data, err := library.PackFast(dir)
	if err != nil {
		return err
	}
	packed := time.Since(start)

	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		addr+"/v1/versions/"+revision, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")

	start = time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server returned %s: %s", resp.Status, body)
	}

	fmt.Fprintf(stdout, "packed %.1f MB in %v, uploaded and loaded in %v\n",
		float64(len(data))/(1<<20), packed.Round(time.Millisecond), time.Since(start).Round(time.Millisecond))
	fmt.Fprintf(stdout, "%s\n", body)
	return nil
}

// roundStats aggregates one round across every chunk and client.
type roundStats struct {
	mu           sync.Mutex
	latencies    []int64
	results      int
	failures     int
	manifests    int
	renderedByte int64
	firstResult  time.Duration
	chunkWall    []time.Duration
}

// oneRound sends every chunk, spread over the requested number of concurrent
// clients, and reports the aggregate. This is the client-side model of running
// behind a Kubernetes Service: each chunk is an independent request that any
// replica can answer.
func oneRound(ctx context.Context, client *http.Client, url string, bodies [][]byte, clients int, round int, stdout io.Writer) error {
	if clients < 1 {
		clients = 1
	}
	start := time.Now()

	stats := &roundStats{firstResult: -1}
	jobs := make(chan []byte)
	errCh := make(chan error, clients)

	var wg sync.WaitGroup
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for body := range jobs {
				if err := sendChunk(ctx, client, url, body, start, stats); err != nil {
					select {
					case errCh <- err:
					default:
					}
					return
				}
			}
		}()
	}
	for _, b := range bodies {
		select {
		case jobs <- b:
		case <-ctx.Done():
		}
	}
	close(jobs)
	wg.Wait()

	select {
	case err := <-errCh:
		return err
	default:
	}

	total := time.Since(start)
	sort.Slice(stats.latencies, func(i, j int) bool { return stats.latencies[i] < stats.latencies[j] })

	fmt.Fprintf(stdout, "\nround %d\n", round)
	fmt.Fprintf(stdout, "  wall clock        %v\n", total.Round(time.Millisecond))
	fmt.Fprintf(stdout, "  first result      %v\n", stats.firstResult.Round(time.Millisecond))
	fmt.Fprintf(stdout, "  results           %d (%d failed)\n", stats.results, stats.failures)
	fmt.Fprintf(stdout, "  throughput        %.1f configs/s\n", float64(stats.results)/total.Seconds())
	fmt.Fprintf(stdout, "  manifests         %d\n", stats.manifests)
	fmt.Fprintf(stdout, "  rendered          %.1f MB (%.1f MB/s)\n",
		float64(stats.renderedByte)/(1<<20), float64(stats.renderedByte)/(1<<20)/total.Seconds())
	fmt.Fprintf(stdout, "  per-config p50    %s\n", ms(pct(stats.latencies, 0.50)))
	fmt.Fprintf(stdout, "  per-config p95    %s\n", ms(pct(stats.latencies, 0.95)))
	fmt.Fprintf(stdout, "  per-config p99    %s\n", ms(pct(stats.latencies, 0.99)))
	if len(stats.chunkWall) > 0 {
		sort.Slice(stats.chunkWall, func(i, j int) bool { return stats.chunkWall[i] < stats.chunkWall[j] })
		fmt.Fprintf(stdout, "  chunk wall p50    %v\n", stats.chunkWall[len(stats.chunkWall)/2].Round(time.Millisecond))
		fmt.Fprintf(stdout, "  chunk wall max    %v\n", stats.chunkWall[len(stats.chunkWall)-1].Round(time.Millisecond))
	}
	return nil
}

func sendChunk(ctx context.Context, client *http.Client, url string, body []byte, roundStart time.Time, stats *roundStats) error {
	chunkStart := time.Now()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("server returned %s: %s", resp.Status, msg)
	}

	// The response is NDJSON, so time-to-first-result is separable from total
	// time. On a chunk that takes seconds those are very different numbers and
	// only one of them is what a caller waits for.
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for scanner.Scan() {
		var env envelope
		if err := json.Unmarshal(scanner.Bytes(), &env); err != nil {
			return fmt.Errorf("decode stream: %w", err)
		}
		if env.Type != "result" {
			continue
		}
		var r result
		if err := json.Unmarshal(env.Result, &r); err != nil {
			return err
		}
		stats.mu.Lock()
		if stats.firstResult < 0 {
			stats.firstResult = time.Since(roundStart)
		}
		stats.results++
		if !r.OK {
			stats.failures++
		}
		stats.manifests += r.Manifests
		stats.renderedByte += int64(r.TotalBytes)
		stats.latencies = append(stats.latencies, r.Micros)
		stats.mu.Unlock()
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read stream: %w", err)
	}

	stats.mu.Lock()
	stats.chunkWall = append(stats.chunkWall, time.Since(chunkStart))
	stats.mu.Unlock()
	return nil
}

func buildConfigs(n int, renderAll bool) []config {
	tiers := []string{"free", "standard", "premium", "enterprise"}
	regions := []string{"eu-west-1", "us-east-1", "ap-south-1", "eu-central-2"}
	featureSets := [][]string{
		{},
		{"observability"},
		{"observability", "mesh"},
		{"observability", "mesh", "search"},
		{"analytics", "cdn"},
		{"observability", "mesh", "search", "analytics", "cdn", "ml"},
	}

	out := make([]config, n)
	for i := range out {
		out[i] = config{
			ID:        fmt.Sprintf("cust-%05d", i),
			Tier:      tiers[i%len(tiers)],
			Region:    regions[i%len(regions)],
			Features:  featureSets[i%len(featureSets)],
			Replicas:  1 + i%5,
			RenderAll: renderAll,
		}
	}
	return out
}

func pct(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(p * float64(len(sorted)))
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

func ms(micros int64) string { return fmt.Sprintf("%.1f ms", float64(micros)/1000) }
