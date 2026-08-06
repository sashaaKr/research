package gitdiff_test

import (
	"context"
	"errors"
	"testing"

	"github.com/sashaakr/research/gitdiff-to-merge-patch/internal/gitdiff"
	"github.com/sashaakr/research/gitdiff-to-merge-patch/internal/gittest"
)

func TestChanges(t *testing.T) {
	ctx := context.Background()
	r := gittest.New(t)
	r.Write("config/a.json", `{"a":1}`)
	r.Write("config/gone.json", `{"x":1}`)
	r.Write("config/old name.json", `{"long":"enough content to be detected as a rename by git"}`)
	r.Commit("base")

	r.Write("config/a.json", `{"a":2}`)
	r.Write("config/new.json", `{"n":1}`)
	r.Remove("config/gone.json")
	r.Git("mv", "config/old name.json", "config/renamed.json")
	r.Commit("head")

	repo := gitdiff.Repo{Dir: r.Dir}
	changes, err := repo.Changes(ctx, "HEAD~1", "HEAD", gitdiff.Options{DetectRenames: true})
	if err != nil {
		t.Fatal(err)
	}

	got := map[string]gitdiff.Change{}
	for _, c := range changes {
		got[c.Path] = c
	}
	if len(got) != 4 {
		t.Fatalf("want 4 changes, got %d: %+v", len(got), changes)
	}
	if c := got["config/a.json"]; c.Status != gitdiff.Modified {
		t.Errorf("a.json: want M, got %q", c.Status)
	}
	if c := got["config/new.json"]; c.Status != gitdiff.Added {
		t.Errorf("new.json: want A, got %q", c.Status)
	}
	if c := got["config/gone.json"]; c.Status != gitdiff.Deleted {
		t.Errorf("gone.json: want D, got %q", c.Status)
	}
	// The space in the old path is the point: with -z it survives verbatim,
	// where the default output would quote and escape it.
	c := got["config/renamed.json"]
	if c.Status != gitdiff.Renamed {
		t.Fatalf("renamed.json: want R, got %q", c.Status)
	}
	if c.OldPath != "config/old name.json" {
		t.Errorf("renamed.json: old path %q", c.OldPath)
	}
	if c.Score == 0 {
		t.Errorf("renamed.json: want a similarity score")
	}
}

// TestChangesMergeBase covers the difference that matters most in a merge
// request pipeline: with two-dot the target branch's own commits show up as
// changes to undo, with three-dot they do not.
func TestChangesMergeBase(t *testing.T) {
	ctx := context.Background()
	r := gittest.New(t)
	r.Write("config/shared.json", `{"a":1}`)
	r.Commit("base")

	r.Git("checkout", "--quiet", "-b", "feature")
	r.Write("config/mine.json", `{"m":1}`)
	r.Commit("feature work")

	r.Git("checkout", "--quiet", "main")
	r.Write("config/theirs.json", `{"t":1}`)
	r.Commit("main moved on")

	repo := gitdiff.Repo{Dir: r.Dir}

	twoDot, err := repo.Changes(ctx, "main", "feature", gitdiff.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(twoDot) != 2 {
		t.Fatalf("two-dot: want mine.json plus a spurious theirs.json deletion, got %+v", twoDot)
	}

	threeDot, err := repo.Changes(ctx, "main", "feature", gitdiff.Options{MergeBase: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(threeDot) != 1 || threeDot[0].Path != "config/mine.json" {
		t.Fatalf("merge base: want only config/mine.json, got %+v", threeDot)
	}
}

func TestBlob(t *testing.T) {
	ctx := context.Background()
	r := gittest.New(t)
	r.Write("config/a.json", `{"a":1}`)
	r.Commit("base")
	r.Write("config/a.json", `{"a":2}`)
	r.Commit("head")

	repo := gitdiff.Repo{Dir: r.Dir}

	base, err := repo.Blob(ctx, "HEAD~1", "config/a.json")
	if err != nil {
		t.Fatal(err)
	}
	if string(base.Data) != `{"a":1}` {
		t.Errorf("base content = %q", base.Data)
	}
	head, err := repo.Blob(ctx, "HEAD", "config/a.json")
	if err != nil {
		t.Fatal(err)
	}
	if base.ID == head.ID || base.ID == "" {
		t.Errorf("blob IDs should differ and be non-empty: %q %q", base.ID, head.ID)
	}

	if _, err := repo.Blob(ctx, "HEAD", "config/missing.json"); !errors.Is(err, gitdiff.ErrNotFound) {
		t.Errorf("missing path: want ErrNotFound, got %v", err)
	}
}

func TestPathspecFilter(t *testing.T) {
	ctx := context.Background()
	r := gittest.New(t)
	r.Write("config/a.json", `{"a":1}`)
	r.Write("README.md", "hello")
	r.Commit("base")
	r.Write("config/a.json", `{"a":2}`)
	r.Write("README.md", "hello world")
	r.Commit("head")

	repo := gitdiff.Repo{Dir: r.Dir}
	changes, err := repo.Changes(ctx, "HEAD~1", "HEAD", gitdiff.Options{Pathspecs: []string{"config"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Path != "config/a.json" {
		t.Fatalf("want only config/a.json, got %+v", changes)
	}
}
