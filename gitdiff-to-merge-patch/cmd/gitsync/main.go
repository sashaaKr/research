// Command gitsync turns a Git revision range into a sync plan: the list of
// HTTP requests that would bring an API in line with the head revision.
//
// It prints the plan and exits. Executing it is deliberately a separate step,
// so a pipeline can render the plan on a merge request and only apply it
// after review.
//
//	gitsync -base origin/main -head HEAD -root config -prefix /api/v1/configs
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path"
	"strings"

	"github.com/sashaakr/research/gitdiff-to-merge-patch/internal/docsync"
	"github.com/sashaakr/research/gitdiff-to-merge-patch/internal/gitdiff"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "gitsync:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("gitsync", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		dir      = fs.String("dir", ".", "directory inside the git working tree")
		base     = fs.String("base", "origin/main", "base revision")
		head     = fs.String("head", "HEAD", "head revision")
		root     = fs.String("root", "", "only sync files under this directory")
		prefix   = fs.String("prefix", "/", "URL prefix for resources")
		twoDot   = fs.Bool("two-dot", false, "compare base..head instead of base...head (merge base)")
		fallback = fs.String("fallback", "replace", "when a merge patch is not faithful: replace|json-patch|error")
		format   = fs.String("format", "json", "output format: json|curl|summary")
		version  = fs.Bool("base-version", true, "attach X-Base-Version headers")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	repo := gitdiff.Repo{Dir: *dir}
	opt := gitdiff.Options{MergeBase: !*twoDot, DetectRenames: true}
	if *root != "" {
		opt.Pathspecs = []string{*root}
	}
	changes, err := repo.Changes(ctx, *base, *head, opt)
	if err != nil {
		return err
	}

	planner := docsync.Planner{
		Resolve:            resolver(*root, *prefix),
		Fallback:           docsync.Fallback(*fallback),
		IncludeBaseVersion: *version,
	}
	plan, err := planner.Plan(ctx, repo, *base, *head, changes)
	if err != nil {
		return err
	}

	switch *format {
	case "json":
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(plan)
	case "curl":
		return writeCurl(stdout, plan)
	case "summary":
		return writeSummary(stdout, plan)
	default:
		return fmt.Errorf("unknown -format %q", *format)
	}
}

// resolver maps "config/eu/rates.json" under root "config" to
// "<prefix>/eu/rates". Files outside root, or with an unsupported extension,
// are skipped -- a README edit in the same commit must not become a request.
func resolver(root, prefix string) func(string) (string, bool) {
	root = strings.Trim(root, "/")
	prefix = strings.TrimSuffix(prefix, "/")
	return func(repoPath string) (string, bool) {
		rel := repoPath
		if root != "" {
			if !strings.HasPrefix(repoPath, root+"/") {
				return "", false
			}
			rel = strings.TrimPrefix(repoPath, root+"/")
		}
		ext := strings.ToLower(path.Ext(rel))
		if ext != ".json" && ext != ".yaml" && ext != ".yml" {
			return "", false
		}
		return prefix + "/" + strings.TrimSuffix(rel, path.Ext(rel)), true
	}
}

func writeCurl(w io.Writer, plan []docsync.Request) error {
	for _, r := range plan {
		if r.Reason != "" {
			fmt.Fprintf(w, "# %s\n", r.Reason)
		}
		fmt.Fprintf(w, "curl -sSf -X %s \"$API_BASE%s\"", r.Method, r.URL)
		for k, v := range r.Headers {
			fmt.Fprintf(w, " \\\n  -H %q", k+": "+v)
		}
		if len(r.Body) > 0 {
			fmt.Fprintf(w, " \\\n  -d %s", shellQuote(string(r.Body)))
		}
		fmt.Fprintln(w)
		fmt.Fprintln(w)
	}
	return nil
}

func writeSummary(w io.Writer, plan []docsync.Request) error {
	if len(plan) == 0 {
		fmt.Fprintln(w, "no changes")
		return nil
	}
	for _, r := range plan {
		fmt.Fprintf(w, "%-6s %-40s %-12s %d bytes\n", r.Method, r.URL, r.Strategy, len(r.Body))
	}
	return nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
