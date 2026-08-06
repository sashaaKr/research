package docsync

import (
	"context"
	"strings"
	"testing"

	jsonpatch "github.com/evanphx/json-patch/v5"

	"github.com/sashaakr/research/gitdiff-to-merge-patch/internal/gitdiff"
	"github.com/sashaakr/research/gitdiff-to-merge-patch/internal/gittest"
)

func TestThreeWay(t *testing.T) {
	tests := []struct {
		name      string
		base      string
		head      string
		live      string
		wantPatch string
		wantDoc   string
	}{
		{
			name:      "ordinary change",
			base:      `{"rate":1.0}`,
			head:      `{"rate":1.2}`,
			live:      `{"rate":1.0}`,
			wantPatch: `{"rate":1.2}`,
			wantDoc:   `{"rate":1.2}`,
		},
		{
			// The case two-way base->head gets wrong: it would send the patch
			// again, on top of a server that already has it.
			name:      "already applied is a no-op",
			base:      `{"rate":1.0}`,
			head:      `{"rate":1.2}`,
			live:      `{"rate":1.2}`,
			wantPatch: `{}`,
			wantDoc:   `{"rate":1.2}`,
		},
		{
			// The case two-way live->head gets wrong: "owner" exists only on
			// the server, so it must survive rather than be deleted.
			name:      "fields the server owns are left alone",
			base:      `{"rate":1.0}`,
			head:      `{"rate":1.2}`,
			live:      `{"rate":1.0,"owner":"platform-team"}`,
			wantPatch: `{"rate":1.2}`,
			wantDoc:   `{"rate":1.2,"owner":"platform-team"}`,
		},
		{
			// And the case only three inputs can express: removed from Git,
			// so it must be deleted -- distinguishable from "owner" above
			// only because it appears in base.
			name:      "removed in git is deleted on the server",
			base:      `{"rate":1.0,"stale":true}`,
			head:      `{"rate":1.0}`,
			live:      `{"rate":1.0,"stale":true,"owner":"platform-team"}`,
			wantPatch: `{"stale":null}`,
			wantDoc:   `{"rate":1.0,"owner":"platform-team"}`,
		},
		{
			name:      "deleting something already gone sends nothing",
			base:      `{"rate":1.0,"stale":true}`,
			head:      `{"rate":1.0}`,
			live:      `{"rate":1.0}`,
			wantPatch: `{}`,
			wantDoc:   `{"rate":1.0}`,
		},
		{
			// Neither commit changed this field, but the server disagrees with
			// both. A two-way diff between commits emits nothing and the drift
			// persists forever; the three-way form repairs it.
			// The literal 1.0 survives as written -- UseNumber does not
			// normalise it to 1.
			name:      "server drifted from both commits and is corrected",
			base:      `{"rate":1.0}`,
			head:      `{"rate":1.0}`,
			live:      `{"rate":99.0}`,
			wantPatch: `{"rate":1.0}`,
			wantDoc:   `{"rate":1.0}`,
		},
		{
			name:      "nested objects merge per field",
			base:      `{"limits":{"cpu":"1","memory":"1Gi"}}`,
			head:      `{"limits":{"cpu":"2","memory":"1Gi"}}`,
			live:      `{"limits":{"cpu":"1","memory":"1Gi","injected":"sidecar"}}`,
			wantPatch: `{"limits":{"cpu":"2"}}`,
			wantDoc:   `{"limits":{"cpu":"2","memory":"1Gi","injected":"sidecar"}}`,
		},
		{
			name:      "nested deletion does not disturb siblings",
			base:      `{"limits":{"cpu":"1","burst":"5"}}`,
			head:      `{"limits":{"cpu":"1"}}`,
			live:      `{"limits":{"cpu":"1","burst":"5","injected":"sidecar"}}`,
			wantPatch: `{"limits":{"burst":null}}`,
			wantDoc:   `{"limits":{"cpu":"1","injected":"sidecar"}}`,
		},
		{
			name:      "new field the server has never seen",
			base:      `{"rate":1.0}`,
			head:      `{"rate":1.0,"region":"eu"}`,
			live:      `{"rate":1.0}`,
			wantPatch: `{"region":"eu"}`,
			wantDoc:   `{"rate":1.0,"region":"eu"}`,
		},
		{
			// Arrays are atomic, as in RFC 7396.
			name:      "arrays are replaced not merged",
			base:      `{"tags":["a","b"]}`,
			head:      `{"tags":["a","b","c"]}`,
			live:      `{"tags":["a","b"]}`,
			wantPatch: `{"tags":["a","b","c"]}`,
			wantDoc:   `{"tags":["a","b","c"]}`,
		},
		{
			name:      "object replacing a scalar is sent wholesale",
			base:      `{"limit":5}`,
			head:      `{"limit":{"soft":5,"hard":10}}`,
			live:      `{"limit":5}`,
			wantPatch: `{"limit":{"hard":10,"soft":5}}`,
			wantDoc:   `{"limit":{"hard":10,"soft":5}}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			patch, want, err := ThreeWay([]byte(tc.base), []byte(tc.head), []byte(tc.live))
			if err != nil {
				t.Fatal(err)
			}
			if !jsonpatch.Equal(patch, []byte(tc.wantPatch)) {
				t.Errorf("patch = %s, want %s", patch, tc.wantPatch)
			}
			if !jsonpatch.Equal(want, []byte(tc.wantDoc)) {
				t.Errorf("expected document = %s, want %s", want, tc.wantDoc)
			}
			// The invariant that makes the whole thing safe: applying the
			// patch to live must produce exactly the promised document.
			got, err := jsonpatch.MergePatch([]byte(tc.live), patch)
			if err != nil {
				t.Fatal(err)
			}
			if !jsonpatch.Equal(got, want) {
				t.Errorf("applying patch to live gave %s, want %s", got, want)
			}
		})
	}
}

// TestThreeWayIsIdempotent checks the property the two-way form lacks:
// planning twice in a row produces nothing the second time.
func TestThreeWayIsIdempotent(t *testing.T) {
	base := []byte(`{"rate":1.0,"stale":true}`)
	head := []byte(`{"rate":1.2}`)
	live := []byte(`{"rate":1.0,"stale":true,"owner":"platform-team"}`)

	patch, want, err := ThreeWay(base, head, live)
	if err != nil {
		t.Fatal(err)
	}
	applied, err := jsonpatch.MergePatch(live, patch)
	if err != nil {
		t.Fatal(err)
	}
	if !jsonpatch.Equal(applied, want) {
		t.Fatalf("first apply gave %s, want %s", applied, want)
	}

	// Same base and head, but the server has now moved to the applied state.
	secondPatch, _, err := ThreeWay(base, head, applied)
	if err != nil {
		t.Fatal(err)
	}
	if !jsonpatch.Equal(secondPatch, []byte(`{}`)) {
		t.Errorf("re-planning produced %s, want an empty patch", secondPatch)
	}
}

func TestThreeWayRejectsNonObjectRoots(t *testing.T) {
	if _, _, err := ThreeWay([]byte(`[]`), []byte(`{}`), []byte(`{}`)); err == nil {
		t.Fatal("want an error for an array root")
	}
	if _, _, err := ThreeWay([]byte(`{}`), []byte(`{}`), []byte(`7`)); err == nil {
		t.Fatal("want an error for a scalar root")
	}
}

// TestThreeWayNullTrapIsStillCaught confirms the verification step survives
// the move to three inputs: an explicit null in the repository still cannot
// be expressed as a merge patch, and the planner must notice.
func TestThreeWayNullTrapIsStillCaught(t *testing.T) {
	p := Planner{Fallback: FallbackReplace}
	body, strategy, reason, err := p.planThreeWay(
		[]byte(`{"retries":3}`),
		[]byte(`{"retries":null}`),
		[]byte(`{"retries":3,"owner":"platform-team"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	if strategy != StrategyReplace {
		t.Fatalf("strategy = %q, want replace", strategy)
	}
	if !strings.Contains(reason, "/retries") {
		t.Errorf("reason = %q, want it to name the offending path", reason)
	}
	// The replacement must carry the server's field too, or the fallback
	// would quietly destroy it.
	if !jsonpatch.Equal(body, []byte(`{"retries":null,"owner":"platform-team"}`)) {
		t.Errorf("body = %s, want the merged document including owner", body)
	}
}

// TestPlanThreeWayEndToEnd drives the planner against a real repository with
// a fake API.
func TestPlanThreeWayEndToEnd(t *testing.T) {
	ctx := context.Background()
	r := gittest.New(t)
	r.Write("config/rates.json", `{"rate":1.0,"stale":true}`)
	r.Write("config/synced.json", `{"a":1}`)
	r.Write("config/missing.json", `{"m":1}`)
	r.Commit("base")
	r.Write("config/rates.json", `{"rate":1.2}`)
	r.Write("config/synced.json", `{"a":2}`)
	r.Write("config/missing.json", `{"m":2}`)
	r.Commit("head")

	// What the API holds. rates has a field the server owns; synced is
	// already at head; missing was never created.
	server := map[string]string{
		"/api/rates":  `{"rate":1.0,"stale":true,"owner":"platform-team"}`,
		"/api/synced": `{"a":2}`,
	}

	p := Planner{
		Resolve: underConfig,
		Fetch: func(_ context.Context, url string) ([]byte, error) {
			doc, ok := server[url]
			if !ok {
				return nil, ErrResourceAbsent
			}
			return []byte(doc), nil
		},
	}

	repo := gitdiff.Repo{Dir: r.Dir}
	changes, err := repo.Changes(ctx, "HEAD~1", "HEAD", gitdiff.Options{})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := p.Plan(ctx, repo, "HEAD~1", "HEAD", changes)
	if err != nil {
		t.Fatal(err)
	}

	byURL := map[string]Request{}
	for _, req := range plan {
		byURL[req.URL] = req
	}
	if len(plan) != 2 {
		t.Fatalf("want 2 requests, got %d: %+v", len(plan), summarise(plan))
	}

	// synced.json changed in Git but the server already agrees: no request.
	if _, ok := byURL["/api/synced"]; ok {
		t.Error("/api/synced already matches head and must not be patched again")
	}

	rates := byURL["/api/rates"]
	if rates.Method != "PATCH" {
		t.Errorf("rates method = %s", rates.Method)
	}
	if !jsonpatch.Equal(rates.Body, []byte(`{"rate":1.2,"stale":null}`)) {
		t.Errorf("rates body = %s, want the rate set and stale deleted, owner untouched", rates.Body)
	}

	// missing.json is absent server-side, so the update becomes a create.
	missing := byURL["/api/missing"]
	if missing.Method != "PUT" || missing.Op != OpCreate {
		t.Errorf("missing: got %s/%s, want PUT/create", missing.Method, missing.Op)
	}
	if !jsonpatch.Equal(missing.Body, []byte(`{"m":2}`)) {
		t.Errorf("missing body = %s", missing.Body)
	}
}
