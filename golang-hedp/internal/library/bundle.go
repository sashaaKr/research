package library

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// Pack turns a library directory into a bundle: the same gzipped tar that
// `git archive <sha>` produces for the same tree.
//
// In production the bundle usually comes from somewhere else - git, an OCI
// registry, an object store - and this function is not in the path. It exists
// so that a directory on disk can be turned into the in-memory form for tests,
// benchmarks and local runs, without a second code path for "loaded from a
// folder" that then behaves subtly differently.
func Pack(dir string) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		return writeEntry(tw, filepath.ToSlash(rel), data)
	})
	if err != nil {
		return nil, fmt.Errorf("pack %s: %w", dir, err)
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// PackFast produces the form a service should actually ship: an uncompressed
// outer tar holding blueprint.yaml plus one gzipped archive per chart.
//
// The layout exists for one reason. Decompression dominates bundle loading and
// gzip is single-threaded per stream, so a single gzip around the whole
// library serialises the most expensive part of the load onto one core. One
// stream per chart lets the loader use all of them. The outer tar is left
// uncompressed because the charts inside are already compressed.
func PackFast(dir string) ([]byte, error) {
	entries, err := os.ReadDir(filepath.Join(dir, "charts"))
	if err != nil {
		return nil, fmt.Errorf("read charts dir: %w", err)
	}

	type packedChart struct {
		name string
		data []byte
		err  error
	}
	results := make(chan packedChart, len(entries))

	var wg sync.WaitGroup
	sem := make(chan struct{}, runtime.GOMAXPROCS(0))
	var chartCount int
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		chartCount++
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			data, err := tarGz(filepath.Join(dir, "charts", name), name)
			results <- packedChart{name, data, err}
		}(e.Name())
	}
	wg.Wait()
	close(results)

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	blueprint, err := os.ReadFile(filepath.Join(dir, "blueprint.yaml"))
	if err != nil {
		return nil, fmt.Errorf("read blueprint: %w", err)
	}
	if err := writeEntry(tw, "blueprint.yaml", blueprint); err != nil {
		return nil, err
	}

	for r := range results {
		if r.err != nil {
			return nil, fmt.Errorf("pack chart %s: %w", r.name, r.err)
		}
		if err := writeEntry(tw, "charts/"+r.name+".tgz", r.data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if chartCount == 0 {
		return nil, fmt.Errorf("no charts found under %s/charts", dir)
	}
	return buf.Bytes(), nil
}

// tarGz packs one chart directory into a gzipped tar rooted at the chart name,
// which is the layout helm package produces and loader.LoadArchive expects.
func tarGz(dir, name string) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		return writeEntry(tw, name+"/"+filepath.ToSlash(rel), data)
	})
	if err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeEntry(tw *tar.Writer, name string, data []byte) error {
	hdr := &tar.Header{
		Name:     name,
		Mode:     0o644,
		Size:     int64(len(data)),
		Typeflag: tar.TypeReg,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

// PackTo writes a bundle to a file.
func PackTo(dir, out string) error {
	data, err := Pack(dir)
	if err != nil {
		return err
	}
	if !strings.HasSuffix(out, ".tgz") && !strings.HasSuffix(out, ".tar.gz") {
		out += ".tgz"
	}
	return os.WriteFile(out, data, 0o644)
}
