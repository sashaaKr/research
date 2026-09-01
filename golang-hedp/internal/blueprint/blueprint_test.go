package blueprint_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sashaakr/research/golang-hedp/internal/blueprint"
)

func write(tb testing.TB, body string) string {
	tb.Helper()
	path := filepath.Join(tb.TempDir(), "blueprint.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		tb.Fatal(err)
	}
	return path
}

// A diamond: d depends on b and c, both of which depend on a. It is the
// smallest graph where a naive "render in listed order" is wrong and where
// wave width is greater than one.
const diamond = `
releases:
  - name: d
    chart: chart-d
    requires: ["top"]
    dependsOn: ["b", "c"]
  - name: b
    chart: chart-b
    requires: ["mid"]
    dependsOn: ["a"]
  - name: c
    chart: chart-c
    requires: ["mid"]
    dependsOn: ["a"]
  - name: a
    chart: chart-a
`

func TestLoadOrdersDependenciesFirst(t *testing.T) {
	bp, err := blueprint.Load(write(t, diamond))
	if err != nil {
		t.Fatal(err)
	}

	names := bp.Names()
	pos := map[string]int{}
	for i, n := range names {
		pos[n] = i
	}
	for _, edge := range [][2]string{{"a", "b"}, {"a", "c"}, {"b", "d"}, {"c", "d"}} {
		if pos[edge[0]] > pos[edge[1]] {
			t.Errorf("%s must be ordered before %s, got %v", edge[0], edge[1], names)
		}
	}
}

func TestPlanPartitionsIntoWaves(t *testing.T) {
	bp, err := blueprint.Load(write(t, diamond))
	if err != nil {
		t.Fatal(err)
	}

	plan := bp.PlanFor([]string{"top", "mid"}, false)
	if plan.Count() != 4 {
		t.Fatalf("expected all 4 releases, got %v", plan.Selected)
	}
	if len(plan.Waves) != 3 {
		t.Fatalf("expected 3 waves (a | b,c | d), got %v", plan.Waves)
	}
	if got := strings.Join(plan.Waves[0], ","); got != "a" {
		t.Errorf("wave 0 = %q, want \"a\"", got)
	}
	if len(plan.Waves[1]) != 2 {
		t.Errorf("wave 1 = %v, want b and c together", plan.Waves[1])
	}
	if got := strings.Join(plan.Waves[2], ","); got != "d" {
		t.Errorf("wave 2 = %q, want \"d\"", got)
	}
	// b and c are independent, so the widest wave is the parallelism ceiling.
	if plan.Width() != 2 {
		t.Errorf("width = %d, want 2", plan.Width())
	}
}

// TestPlanPullsInDependenciesPastTheirGates is the behaviour that makes
// dependsOn meaningful: a is not selected by any feature the customer has, but
// b needs it, so it renders anyway - and is reported as pulled in.
func TestPlanPullsInDependenciesPastTheirGates(t *testing.T) {
	bp, err := blueprint.Load(write(t, `
releases:
  - name: gated
    chart: chart-gated
    requires: ["never-enabled"]
  - name: needed
    chart: chart-needed
    requires: ["also-never"]
  - name: wanted
    chart: chart-wanted
    requires: ["yes"]
    dependsOn: ["needed"]
`))
	if err != nil {
		t.Fatal(err)
	}

	plan := bp.PlanFor([]string{"yes"}, false)

	if plan.Count() != 2 {
		t.Fatalf("expected wanted + needed, got %v", plan.Selected)
	}
	if len(plan.PulledIn) != 1 || plan.PulledIn[0] != "needed" {
		t.Errorf("PulledIn = %v, want [needed]", plan.PulledIn)
	}
	for _, n := range plan.Selected {
		if n == "gated" {
			t.Error("a release nothing depends on and no feature enables must not render")
		}
	}
}

func TestPlanForAllIgnoresGates(t *testing.T) {
	bp, err := blueprint.Load(write(t, diamond))
	if err != nil {
		t.Fatal(err)
	}
	if plan := bp.PlanFor(nil, true); plan.Count() != 4 {
		t.Errorf("render-all selected %d releases, want 4", plan.Count())
	}
	if plan := bp.PlanFor(nil, false); plan.Count() != 1 {
		t.Errorf("no features selected %d releases, want just the ungated one", plan.Count())
	}
}

// A cycle must be rejected at load. Left in, it would surface later as a
// release waiting on an export that can never arrive.
func TestLoadRejectsCycles(t *testing.T) {
	_, err := blueprint.Load(write(t, `
releases:
  - name: a
    chart: c
    dependsOn: ["b"]
  - name: b
    chart: c
    dependsOn: ["a"]
`))
	if err == nil {
		t.Fatal("expected a cycle to be rejected")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("error should name the cycle, got: %v", err)
	}
}

func TestLoadRejectsMalformedGraphs(t *testing.T) {
	cases := map[string]string{
		"unknown dependency": `
releases:
  - name: a
    chart: c
    dependsOn: ["nope"]
`,
		"duplicate release": `
releases:
  - name: a
    chart: c
  - name: a
    chart: c
`,
		"missing chart": `
releases:
  - name: a
`,
		"no releases": `releases: []`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := blueprint.Load(write(t, body)); err == nil {
				t.Error("expected load to fail")
			}
		})
	}
}

func TestValidateReportsMissingCharts(t *testing.T) {
	bp, err := blueprint.Load(write(t, diamond))
	if err != nil {
		t.Fatal(err)
	}

	err = bp.Validate(func(name string) bool { return name != "chart-c" })
	if err == nil {
		t.Fatal("expected a missing chart to be reported")
	}
	if !strings.Contains(err.Error(), "chart-c") {
		t.Errorf("error should name the missing chart, got: %v", err)
	}

	if err := bp.Validate(func(string) bool { return true }); err != nil {
		t.Errorf("expected success when every chart exists, got %v", err)
	}
}

func TestFeaturesAreDeduplicatedAndSorted(t *testing.T) {
	bp, err := blueprint.Load(write(t, diamond))
	if err != nil {
		t.Fatal(err)
	}
	got := bp.Features()
	want := []string{"mid", "top"}
	if len(got) != len(want) {
		t.Fatalf("features = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("features = %v, want %v", got, want)
		}
	}
}
