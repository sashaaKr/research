// Command dumpmanifests renders one customer configuration and writes every
// manifest as JSON on stdout, keyed by release and normalised template name.
//
// It exists for one job: proving that the Go service and the Rust port render
// the same bytes. A performance comparison between two programs doing
// different amounts of work is worthless, so the equivalence has to be
// checked, not assumed.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sashaakr/research/golang-hedp/internal/blueprint"
	"github.com/sashaakr/research/golang-hedp/internal/catalog"
	"github.com/sashaakr/research/golang-hedp/internal/customer"
	"github.com/sashaakr/research/golang-hedp/internal/render"
)

func main() {
	library := flag.String("library", "testdata/library", "library directory")
	id := flag.String("id", "acme", "customer id")
	tier := flag.String("tier", "premium", "customer tier")
	region := flag.String("region", "eu-west-1", "customer region")
	features := flag.String("features", "", "comma-separated features")
	all := flag.Bool("all", false, "render the entire library")
	flag.Parse()

	cat, err := catalog.Load(filepath.Join(*library, "charts"))
	if err != nil {
		fail(err)
	}
	bp, err := blueprint.Load(filepath.Join(*library, "blueprint.yaml"))
	if err != nil {
		fail(err)
	}
	r, err := render.New(cat, bp, render.Options{ReleaseConcurrency: 1, IncludeManifests: true, CachedEngine: true})
	if err != nil {
		fail(err)
	}

	var feats []string
	if *features != "" {
		feats = strings.Split(*features, ",")
	}
	res := r.Render(context.Background(), customer.Config{
		ID: *id, Tier: *tier, Region: *region, Features: feats, RenderAll: *all,
	})
	if len(res.Problems) > 0 {
		fail(fmt.Errorf("validation: %v", res.Problems))
	}

	out := map[string]string{}
	for _, rel := range res.Releases {
		if rel.Error != "" {
			fail(fmt.Errorf("release %s: %s", rel.Name, rel.Error))
		}
		for name, body := range rel.Files {
			out[rel.Name+"/"+normalise(name)] = body
		}
	}
	if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
		fail(err)
	}
	fmt.Fprintf(os.Stderr, "%d releases, %d manifests, %d bytes\n", res.Planned, res.Manifests, res.TotalBytes)
}

// normalise strips the chart path and extension so a Go "…/templates/foo.yaml"
// and a Jinja "foo.j2" land on the same key.
func normalise(name string) string {
	if i := strings.Index(name, "/templates/"); i >= 0 {
		name = name[i+len("/templates/"):]
	}
	name = strings.TrimSuffix(name, ".yaml")
	name = strings.TrimSuffix(name, ".j2")
	return name
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "dumpmanifests:", err)
	os.Exit(1)
}
