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
	"time"
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
	)
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	body, err := json.Marshal(map[string]any{
		"configs":     buildConfigs(*count, *renderAll),
		"concurrency": *concurrency,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "request: %d configs, %.1f KB body\n", *count, float64(len(body))/1024)

	client := &http.Client{Timeout: 30 * time.Minute}

	for round := 1; round <= *rounds; round++ {
		if err := oneRound(ctx, client, *addr, body, round, stdout); err != nil {
			return err
		}
	}
	return nil
}

func oneRound(ctx context.Context, client *http.Client, addr string, body []byte, round int, stdout io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, addr+"/v1/render/bulk", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("server returned %s: %s", resp.Status, msg)
	}

	var (
		first        time.Duration
		latencies    []int64
		results      int
		failures     int
		manifests    int
		renderedByte int64
		summaryRaw   json.RawMessage
	)

	// The response is NDJSON, so the client can account for time-to-first-result
	// separately from total time. On a batch that takes a minute, those are
	// very different numbers and only one of them is the user's experience.
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for scanner.Scan() {
		var env envelope
		if err := json.Unmarshal(scanner.Bytes(), &env); err != nil {
			return fmt.Errorf("decode stream: %w", err)
		}
		switch env.Type {
		case "result":
			if first == 0 {
				first = time.Since(start)
			}
			var r result
			if err := json.Unmarshal(env.Result, &r); err != nil {
				return err
			}
			results++
			if !r.OK {
				failures++
			}
			manifests += r.Manifests
			renderedByte += int64(r.TotalBytes)
			latencies = append(latencies, r.Micros)
		case "summary":
			summaryRaw = env.Summary
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read stream: %w", err)
	}

	total := time.Since(start)
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })

	fmt.Fprintf(stdout, "\nround %d\n", round)
	fmt.Fprintf(stdout, "  wall clock        %v\n", total.Round(time.Millisecond))
	fmt.Fprintf(stdout, "  first result      %v\n", first.Round(time.Millisecond))
	fmt.Fprintf(stdout, "  results           %d (%d failed)\n", results, failures)
	fmt.Fprintf(stdout, "  throughput        %.1f configs/s\n", float64(results)/total.Seconds())
	fmt.Fprintf(stdout, "  manifests         %d\n", manifests)
	fmt.Fprintf(stdout, "  rendered          %.1f MB (%.1f MB/s)\n",
		float64(renderedByte)/(1<<20), float64(renderedByte)/(1<<20)/total.Seconds())
	fmt.Fprintf(stdout, "  server-side p50   %s\n", ms(pct(latencies, 0.50)))
	fmt.Fprintf(stdout, "  server-side p95   %s\n", ms(pct(latencies, 0.95)))
	fmt.Fprintf(stdout, "  server-side p99   %s\n", ms(pct(latencies, 0.99)))
	if len(latencies) > 0 {
		fmt.Fprintf(stdout, "  server-side max   %s\n", ms(latencies[len(latencies)-1]))
	}
	if len(summaryRaw) > 0 {
		fmt.Fprintf(stdout, "  server summary    %s\n", summaryRaw)
	}
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
