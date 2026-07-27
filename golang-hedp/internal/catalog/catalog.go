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
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
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

// LoadBundle builds a Catalog from a tar of the chart library, with no
// filesystem involved at any point.
//
// This is the path that matters when release sets are versioned per commit:
// the bundle is whatever `git archive <sha>` or an OCI layer hands you, it
// arrives over the network as bytes, and it is loaded straight out of RAM.
//
// Two layouts are accepted, and the difference is worth understanding:
//
//   - charts/<name>/...      loose files, as a checkout has them.
//   - charts/<name>.tgz      packed Helm charts, as `helm package` produces.
//
// The packed layout loads several times faster. Decompression is the dominant
// cost of reading a bundle (measured at 87% of it), gzip is single-threaded
// per stream, and one archive per chart is one stream per chart - so the work
// parallelises across cores. A single gzip wrapping the whole bundle cannot.
// Use an uncompressed outer tar with packed charts inside; compressing
// already-compressed charts buys nothing.
//
// Any leading path component is tolerated, because `git archive` prefixes
// everything with the tree name and OCI layers usually do not.
func LoadBundle(data []byte) (*Catalog, error) {
	start := time.Now()

	loose, packed, err := splitBundle(data)
	if err != nil {
		return nil, err
	}
	if len(loose)+len(packed) == 0 {
		return nil, fmt.Errorf("bundle contains no charts under charts/")
	}

	type loaded struct {
		name string
		c    *chart.Chart
		err  error
	}
	results := make(chan loaded, len(loose)+len(packed))

	var wg sync.WaitGroup
	sem := make(chan struct{}, runtime.GOMAXPROCS(0))

	for name, files := range loose {
		wg.Add(1)
		go func(name string, files []*loader.BufferedFile) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			c, err := loader.LoadFiles(files)
			results <- loaded{name, c, err}
		}(name, files)
	}
	for name, body := range packed {
		wg.Add(1)
		go func(name string, body []byte) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			c, err := loader.LoadArchive(bytes.NewReader(body))
			results <- loaded{name, c, err}
		}(name, body)
	}
	wg.Wait()
	close(results)

	charts := make(map[string]*chart.Chart, len(loose)+len(packed))
	names := make([]string, 0, len(loose)+len(packed))
	for r := range results {
		if r.err != nil {
			return nil, fmt.Errorf("load chart %s: %w", r.name, r.err)
		}
		// A packed chart carries its own name in Chart.yaml; prefer it over the
		// filename so `foo-1.2.3.tgz` registers as "foo".
		name := r.name
		if r.c.Metadata != nil && r.c.Metadata.Name != "" {
			name = r.c.Metadata.Name
		}
		charts[name] = r.c
		names = append(names, name)
	}
	sort.Strings(names)

	c := &Catalog{charts: charts, names: names}
	c.stats = computeStats(charts)
	c.stats.LoadDuration = time.Since(start)
	c.stats.LoadMillis = c.stats.LoadDuration.Milliseconds()
	return c, nil
}

// BundleBlueprint extracts blueprint.yaml from a bundle. It is separate from
// LoadBundle so the caller decides what to do with a bundle that has no
// blueprint - a chart-only bundle is a legitimate thing to want.
func BundleBlueprint(data []byte) ([]byte, error) {
	var found []byte
	err := walkBundle(data, func(name string, body []byte) {
		if path.Base(name) == "blueprint.yaml" && found == nil {
			found = body
		}
	})
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, fmt.Errorf("bundle contains no blueprint.yaml")
	}
	return found, nil
}

// splitBundle sorts the archive's entries into loose per-chart files (paths
// rewritten relative to their chart root, which is what loader.LoadFiles
// wants) and packed per-chart archives.
func splitBundle(data []byte) (loose map[string][]*loader.BufferedFile, packed map[string][]byte, err error) {
	loose = map[string][]*loader.BufferedFile{}
	packed = map[string][]byte{}

	err = walkBundle(data, func(name string, body []byte) {
		parts := strings.Split(name, "/")
		// Find the "charts" segment; anything before it is an archive prefix.
		idx := -1
		for i, p := range parts {
			if p == "charts" {
				idx = i
				break
			}
		}
		if idx < 0 || idx+1 >= len(parts) {
			return
		}

		entry := parts[idx+1]
		if idx+2 == len(parts) {
			// charts/<something> with nothing below it: a packed chart, or junk.
			if base, ok := strings.CutSuffix(entry, ".tgz"); ok {
				packed[base] = body
			} else if base, ok := strings.CutSuffix(entry, ".tar.gz"); ok {
				packed[base] = body
			}
			return
		}

		rel := strings.Join(parts[idx+2:], "/")
		if rel == "" {
			return
		}
		loose[entry] = append(loose[entry], &loader.BufferedFile{Name: rel, Data: body})
	})
	if err != nil {
		return nil, nil, err
	}
	return loose, packed, nil
}

// walkBundle reads the outer tar, transparently handling a gzipped one.
func walkBundle(data []byte, visit func(name string, body []byte)) error {
	var r io.Reader = bytes.NewReader(data)

	// gzip magic. An uncompressed outer tar is the faster and preferred form,
	// but a plain `git archive --format=tar.gz` has to keep working.
	if len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b {
		gz, err := gzip.NewReader(r)
		if err != nil {
			return fmt.Errorf("bundle is not readable gzip: %w", err)
		}
		defer gz.Close()
		r = gz
	}

	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read bundle: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if hdr.Size > maxBundleFileBytes {
			return fmt.Errorf("bundle entry %s is %d bytes, over the %d limit", hdr.Name, hdr.Size, maxBundleFileBytes)
		}
		body := make([]byte, hdr.Size)
		if _, err := io.ReadFull(tr, body); err != nil {
			return fmt.Errorf("read %s from bundle: %w", hdr.Name, err)
		}
		visit(path.Clean(hdr.Name), body)
	}
	return nil
}

// maxBundleFileBytes stops a malicious or corrupt archive from claiming a
// single file is larger than memory.
const maxBundleFileBytes = 256 << 20

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
