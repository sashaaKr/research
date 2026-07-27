package library_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"path"
	"strings"
	"testing"

	"github.com/sashaakr/research/golang-hedp/internal/blueprint"
	"github.com/sashaakr/research/golang-hedp/internal/catalog"
	"github.com/sashaakr/research/golang-hedp/internal/render"
)

// newDirRenderer builds a renderer the old way, straight off the filesystem,
// so the bundle path can be compared against it.
func newDirRenderer(tb testing.TB, root string) *render.Renderer {
	tb.Helper()
	cat, err := catalog.Load(path.Join(root, "charts"))
	if err != nil {
		tb.Fatal(err)
	}
	bp, err := blueprint.Load(path.Join(root, "blueprint.yaml"))
	if err != nil {
		tb.Fatal(err)
	}
	r, err := render.New(cat, bp, render.Options{ReleaseConcurrency: 1, IncludeManifests: true})
	if err != nil {
		tb.Fatal(err)
	}
	return r
}

// rewriteChartValue produces a second bundle that differs from the first in
// one value, standing in for "the next commit". It rewrites the tar in memory
// rather than copying 55 MB of files around.
func rewriteChartValue(tb testing.TB, dir, old, replacement string) []byte {
	tb.Helper()
	base := bundle(tb)

	gzr, err := gzip.NewReader(bytes.NewReader(base))
	if err != nil {
		tb.Fatal(err)
	}
	defer gzr.Close()

	var out bytes.Buffer
	gzw := gzip.NewWriter(&out)
	tw := tar.NewWriter(gzw)

	tr := tar.NewReader(gzr)
	var rewritten int
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			tb.Fatal(err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			tb.Fatal(err)
		}
		if path.Base(hdr.Name) == "values.yaml" && bytes.Contains(body, []byte(old)) {
			body = []byte(strings.Replace(string(body), old, replacement, 1))
			rewritten++
		}
		nh := *hdr
		nh.Size = int64(len(body))
		if err := tw.WriteHeader(&nh); err != nil {
			tb.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			tb.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		tb.Fatal(err)
	}
	if err := gzw.Close(); err != nil {
		tb.Fatal(err)
	}
	if rewritten == 0 {
		tb.Fatalf("no values.yaml contained %q, the test would prove nothing", old)
	}
	return out.Bytes()
}
