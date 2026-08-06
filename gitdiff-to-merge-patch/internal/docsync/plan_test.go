package docsync

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	jsonpatch "github.com/evanphx/json-patch/v5"

	"github.com/sashaakr/research/gitdiff-to-merge-patch/internal/gitdiff"
	"github.com/sashaakr/research/gitdiff-to-merge-patch/internal/gittest"
)

func TestBuildPatchMergePatch(t *testing.T) {
	tests := []struct {
		name     string
		original string
		target   string
		want     string
	}{
		{
			name:     "set a field",
			original: `{"a":1,"b":2}`,
			target:   `{"a":9,"b":2}`,
			want:     `{"a":9}`,
		},
		{
			name:     "add a field",
			original: `{"a":1}`,
			target:   `{"a":1,"b":2}`,
			want:     `{"b":2}`,
		},
		{
			name:     "remove a field becomes null",
			original: `{"a":1,"b":2}`,
			target:   `{"a":1}`,
			want:     `{"b":null}`,
		},
		{
			// The recursion is the reason merge patch is pleasant to read:
			// only the changed leaf appears, not the whole subtree.
			name:     "nested change touches only the leaf",
			original: `{"limits":{"cpu":"1","memory":"1Gi"},"name":"x"}`,
			target:   `{"limits":{"cpu":"2","memory":"1Gi"},"name":"x"}`,
			want:     `{"limits":{"cpu":"2"}}`,
		},
		{
			// Arrays have no element-level semantics in RFC 7396: changing
			// one entry resends the entire array.
			name:     "array is replaced wholesale",
			original: `{"tags":["a","b","c"]}`,
			target:   `{"tags":["a","z","c"]}`,
			want:     `{"tags":["a","z","c"]}`,
		},
		{
			name:     "empty object member is preserved not dropped",
			original: `{"a":{"b":1}}`,
			target:   `{"a":{}}`,
			want:     `{"a":{"b":null}}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := Planner{}
			body, strategy, reason, err := p.buildPatch([]byte(tc.original), []byte(tc.target))
			if err != nil {
				t.Fatal(err)
			}
			if strategy != StrategyMergePatch {
				t.Fatalf("strategy = %q (reason %q), want merge-patch", strategy, reason)
			}
			if !jsonpatch.Equal(body, []byte(tc.want)) {
				t.Errorf("patch = %s, want %s", body, tc.want)
			}
			// Whatever the shape, the patch must reproduce the target.
			got, err := jsonpatch.MergePatch([]byte(tc.original), body)
			if err != nil {
				t.Fatal(err)
			}
			if !jsonpatch.Equal(got, []byte(tc.target)) {
				t.Errorf("applying patch gave %s, want %s", got, tc.target)
			}
		})
	}
}

// TestBuildPatchNullTrap is the case the whole design exists for. RFC 7396
// reserves null as "delete this member", so a document that legitimately
// holds null cannot be reached by a merge patch. Generating one anyway
// silently drops the field on the server.
func TestBuildPatchNullTrap(t *testing.T) {
	const (
		original = `{"retries":3,"note":"hi"}`
		target   = `{"retries":null,"note":"hi"}`
	)

	// Demonstrate the corruption the verification step catches.
	naive, err := jsonpatch.CreateMergePatch([]byte(original), []byte(target))
	if err != nil {
		t.Fatal(err)
	}
	applied, err := jsonpatch.MergePatch([]byte(original), naive)
	if err != nil {
		t.Fatal(err)
	}
	if jsonpatch.Equal(applied, []byte(target)) {
		t.Fatal("expected the naive merge patch to lose the null field")
	}
	if strings.Contains(string(applied), "retries") {
		t.Fatalf("expected retries to be deleted, got %s", applied)
	}

	t.Run("replace fallback", func(t *testing.T) {
		p := Planner{Fallback: FallbackReplace}
		body, strategy, reason, err := p.buildPatch([]byte(original), []byte(target))
		if err != nil {
			t.Fatal(err)
		}
		if strategy != StrategyReplace {
			t.Fatalf("strategy = %q, want replace", strategy)
		}
		if !jsonpatch.Equal(body, []byte(target)) {
			t.Errorf("body = %s, want the full target document", body)
		}
		if !strings.Contains(reason, "/retries") {
			t.Errorf("reason should point at the offending path, got %q", reason)
		}
	})

	t.Run("json patch fallback", func(t *testing.T) {
		p := Planner{Fallback: FallbackJSONPatch}
		body, strategy, _, err := p.buildPatch([]byte(original), []byte(target))
		if err != nil {
			t.Fatal(err)
		}
		if strategy != StrategyJSONPatch {
			t.Fatalf("strategy = %q, want json-patch", strategy)
		}
		// RFC 6902 has no null ambiguity: "replace" carries the value.
		var ops []map[string]any
		if err := json.Unmarshal(body, &ops); err != nil {
			t.Fatal(err)
		}
		if len(ops) != 1 || ops[0]["op"] != "replace" || ops[0]["path"] != "/retries" {
			t.Fatalf("unexpected ops: %s", body)
		}
		decoded, err := jsonpatch.DecodePatch(body)
		if err != nil {
			t.Fatal(err)
		}
		got, err := decoded.Apply([]byte(original))
		if err != nil {
			t.Fatal(err)
		}
		if !jsonpatch.Equal(got, []byte(target)) {
			t.Errorf("applying json patch gave %s, want %s", got, target)
		}
	})

	t.Run("error fallback", func(t *testing.T) {
		p := Planner{Fallback: FallbackError}
		if _, _, _, err := p.buildPatch([]byte(original), []byte(target)); err == nil {
			t.Fatal("want an error")
		}
	})
}

func TestPlan(t *testing.T) {
	ctx := context.Background()
	r := gittest.New(t)
	r.Write("config/rates.json", `{"eur": 1.1, "usd": 1.0, "stale": true}`)
	r.Write("config/limits.yaml", "cpu: 1\nmemory: 1Gi\n")
	r.Write("config/dropme.json", `{"x":1}`)
	r.Write("config/reformat.json", `{"b":2,"a":1}`)
	r.Write("README.md", "not a document")
	r.Commit("base")

	r.Write("config/rates.json", `{"eur": 1.2, "usd": 1.0}`)            // change + delete
	r.Write("config/limits.yaml", "# a comment\nmemory: 1Gi\ncpu: 1\n") // no semantic change
	r.Remove("config/dropme.json")
	r.Write("config/reformat.json", "{\n  \"a\": 1,\n  \"b\": 2\n}\n") // whitespace + key order only
	r.Write("config/new.yaml", "enabled: true\n")
	r.Write("README.md", "still not a document")
	r.Commit("head")

	repo := gitdiff.Repo{Dir: r.Dir}
	changes, err := repo.Changes(ctx, "HEAD~1", "HEAD", gitdiff.Options{DetectRenames: true})
	if err != nil {
		t.Fatal(err)
	}

	p := Planner{
		Resolve:            underConfig,
		IncludeBaseVersion: true,
	}
	plan, err := p.Plan(ctx, repo, "HEAD~1", "HEAD", changes)
	if err != nil {
		t.Fatal(err)
	}

	byURL := map[string]Request{}
	for _, req := range plan {
		byURL[req.URL] = req
	}
	if len(plan) != 3 {
		t.Fatalf("want 3 requests, got %d: %+v", len(plan), summarise(plan))
	}

	// README.md is not a document; the YAML comment edit and the JSON
	// reformat are not semantic changes. None of them may reach the API.
	for _, unwanted := range []string{"/api/README", "/api/limits", "/api/reformat"} {
		if _, ok := byURL[unwanted]; ok {
			t.Errorf("%s should not have produced a request", unwanted)
		}
	}

	rates := byURL["/api/rates"]
	if rates.Method != "PATCH" || rates.Strategy != StrategyMergePatch {
		t.Errorf("rates: got %s/%s", rates.Method, rates.Strategy)
	}
	if !jsonpatch.Equal(rates.Body, []byte(`{"eur":1.2,"stale":null}`)) {
		t.Errorf("rates body = %s", rates.Body)
	}
	if rates.Headers["Content-Type"] != MediaTypeMergePatch {
		t.Errorf("rates content type = %q", rates.Headers["Content-Type"])
	}
	if rates.Headers[BaseVersionHeader] == "" {
		t.Errorf("rates: missing %s", BaseVersionHeader)
	}

	created := byURL["/api/new"]
	if created.Method != "PUT" || created.Op != OpCreate {
		t.Errorf("new: got %s/%s", created.Method, created.Op)
	}
	if !jsonpatch.Equal(created.Body, []byte(`{"enabled":true}`)) {
		t.Errorf("new body = %s", created.Body)
	}

	deleted := byURL["/api/dropme"]
	if deleted.Method != "DELETE" || len(deleted.Body) != 0 {
		t.Errorf("dropme: got %s with body %s", deleted.Method, deleted.Body)
	}
	if deleted.Headers[BaseVersionHeader] == "" {
		t.Errorf("dropme: missing %s", BaseVersionHeader)
	}
}

// TestPlanRenameSplitsIntoDeleteAndCreate documents that when the filename is
// the resource identity, a rename cannot be a patch.
func TestPlanRenameSplitsIntoDeleteAndCreate(t *testing.T) {
	ctx := context.Background()
	r := gittest.New(t)
	r.Write("config/old.json", `{"description":"a document long enough for rename detection to fire","n":1}`)
	r.Commit("base")
	r.Git("mv", "config/old.json", "config/new.json")
	r.Commit("head")

	repo := gitdiff.Repo{Dir: r.Dir}
	changes, err := repo.Changes(ctx, "HEAD~1", "HEAD", gitdiff.Options{DetectRenames: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Status != gitdiff.Renamed {
		t.Fatalf("expected a rename, got %+v", changes)
	}

	plan, err := Planner{Resolve: underConfig}.Plan(ctx, repo, "HEAD~1", "HEAD", changes)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 2 {
		t.Fatalf("want DELETE then PUT, got %+v", summarise(plan))
	}
	if plan[0].Method != "DELETE" || plan[0].URL != "/api/old" {
		t.Errorf("first request = %s %s", plan[0].Method, plan[0].URL)
	}
	if plan[1].Method != "PUT" || plan[1].URL != "/api/new" {
		t.Errorf("second request = %s %s", plan[1].Method, plan[1].URL)
	}
}

// TestPlanRenameWithinOneResource covers the other mapping: when the URL is
// derived from the document's contents rather than its filename, moving the
// file is invisible to the API and only the content change is sent.
func TestPlanRenameWithinOneResource(t *testing.T) {
	ctx := context.Background()
	r := gittest.New(t)
	// Pretty-printed on purpose. Git's similarity index is line-based, so a
	// minified single-line document that is renamed *and* edited scores 0%
	// similar and is reported as a delete plus an add. See the README.
	r.Write("config/a.json", "{\n  \"id\": \"stable\",\n  \"value\": 1,\n  \"filler\": \"padding\"\n}\n")
	r.Commit("base")
	r.Write("config/a.json", "{\n  \"id\": \"stable\",\n  \"value\": 2,\n  \"filler\": \"padding\"\n}\n")
	r.Git("mv", "config/a.json", "config/b.json")
	r.Commit("head")

	repo := gitdiff.Repo{Dir: r.Dir}
	changes, err := repo.Changes(ctx, "HEAD~1", "HEAD", gitdiff.Options{DetectRenames: true})
	if err != nil {
		t.Fatal(err)
	}

	// Both filenames map to the same resource.
	byContentID := func(string) (string, bool) { return "/api/stable", true }
	plan, err := Planner{Resolve: byContentID}.Plan(ctx, repo, "HEAD~1", "HEAD", changes)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 1 {
		t.Fatalf("want one PATCH, got %+v", summarise(plan))
	}
	if plan[0].Method != "PATCH" || !jsonpatch.Equal(plan[0].Body, []byte(`{"value":2}`)) {
		t.Errorf("got %s %s", plan[0].Method, plan[0].Body)
	}
}

func TestPlanRequiresResolve(t *testing.T) {
	if _, err := (Planner{}).Plan(context.Background(), gitdiff.Repo{}, "a", "b", nil); err == nil {
		t.Fatal("want an error when Resolve is nil")
	}
}

func underConfig(repoPath string) (string, bool) {
	rest, ok := strings.CutPrefix(repoPath, "config/")
	if !ok {
		return "", false
	}
	for _, ext := range []string{".json", ".yaml", ".yml"} {
		if base, ok := strings.CutSuffix(rest, ext); ok {
			return "/api/" + base, true
		}
	}
	return "", false
}

func summarise(plan []Request) []string {
	out := make([]string, len(plan))
	for i, r := range plan {
		out[i] = r.Method + " " + r.URL + " (" + string(r.Strategy) + ")"
	}
	return out
}
