// Package catalog loads the chart library into memory once, at startup.
//
// This is the single most important decision in the service. Helm's CLI loads
// a chart from disk for every `helm template` invocation; a service that did
// the same per request would spend all of its time in os.ReadFile and YAML
// parsing. Charts are immutable after load, and Helm's render path deep-copies
// chart values before touching them (see chartutil.coalesceValues), so one
// *chart.Chart can be rendered concurrently by any number of goroutines.
package catalog

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
)

// Catalog is an immutable, in-memory set of charts keyed by directory name.
type Catalog struct {
	charts map[string]*chart.Chart
	names  []string
	stats  Stats
}

// Stats describes what was loaded. The byte split matters more than the total:
// only TemplateBytes is re-parsed on every render.
type Stats struct {
	Charts        int           `json:"charts"`
	TemplateFiles int           `json:"template_files"`
	TemplateBytes int64         `json:"template_bytes"`
	FileFiles     int           `json:"file_files"`
	FileBytes     int64         `json:"file_bytes"`
	CRDFiles      int           `json:"crd_files"`
	CRDBytes      int64         `json:"crd_bytes"`
	TotalBytes    int64         `json:"total_bytes"`
	LoadDuration  time.Duration `json:"-"`
	LoadMillis    int64         `json:"load_millis"`
}

// Load reads every immediate subdirectory of root as a chart. Charts are
// loaded in parallel because startup time is dominated by I/O and YAML
// parsing, and a 55 MB library is slow enough to notice.
func Load(root string) (*Catalog, error) {
	start := time.Now()

	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read chart root %s: %w", root, err)
	}

	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	if len(dirs) == 0 {
		return nil, fmt.Errorf("no charts found under %s", root)
	}
	sort.Strings(dirs)

	var (
		mu      sync.Mutex
		charts  = make(map[string]*chart.Chart, len(dirs))
		loadErr error
		wg      sync.WaitGroup
		sem     = make(chan struct{}, 8)
	)
	for _, name := range dirs {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			c, err := loader.LoadDir(filepath.Join(root, name))
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if loadErr == nil {
					loadErr = fmt.Errorf("load chart %s: %w", name, err)
				}
				return
			}
			charts[name] = c
		}(name)
	}
	wg.Wait()
	if loadErr != nil {
		return nil, loadErr
	}

	c := &Catalog{charts: charts, names: dirs}
	c.stats = computeStats(charts)
	c.stats.LoadDuration = time.Since(start)
	c.stats.LoadMillis = c.stats.LoadDuration.Milliseconds()
	return c, nil
}

func computeStats(charts map[string]*chart.Chart) Stats {
	var s Stats
	s.Charts = len(charts)
	for _, c := range charts {
		for _, t := range c.Templates {
			s.TemplateFiles++
			s.TemplateBytes += int64(len(t.Data))
		}
		// Everything else the loader picked up lands in Files, including the
		// crds/ tree. Split it out: CRDs are never opened by the engine, plain
		// files are only opened if a template calls .Files.
		for _, f := range c.Files {
			if strings.HasPrefix(f.Name, "crds/") {
				s.CRDFiles++
				s.CRDBytes += int64(len(f.Data))
				continue
			}
			s.FileFiles++
			s.FileBytes += int64(len(f.Data))
		}
	}
	s.TotalBytes = s.TemplateBytes + s.FileBytes + s.CRDBytes
	return s
}

// Chart returns the named chart. The returned value must be treated as
// read-only; it is shared by every in-flight request.
func (c *Catalog) Chart(name string) (*chart.Chart, bool) {
	ch, ok := c.charts[name]
	return ch, ok
}

// Names returns the chart names in sorted order.
func (c *Catalog) Names() []string { return c.names }

// Stats returns what was loaded.
func (c *Catalog) Stats() Stats { return c.stats }
