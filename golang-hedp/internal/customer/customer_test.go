package customer_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/sashaakr/research/golang-hedp/internal/customer"
)

var known = map[string]bool{"observability": true, "mesh": true, "search": true}

func valid() customer.Config {
	return customer.Config{
		ID:       "acme-corp",
		Tier:     "premium",
		Region:   "eu-west-1",
		Features: []string{"observability", "mesh"},
		Replicas: 3,
	}
}

func TestValidConfigHasNoProblems(t *testing.T) {
	if problems := valid().Valid(known); len(problems) > 0 {
		t.Errorf("expected no problems, got %v", problems)
	}
}

func TestFieldValidation(t *testing.T) {
	cases := []struct {
		name   string
		field  string
		mutate func(*customer.Config)
	}{
		{"empty id", "id", func(c *customer.Config) { c.ID = "" }},
		{"uppercase id", "id", func(c *customer.Config) { c.ID = "ACME" }},
		{"underscore in id", "id", func(c *customer.Config) { c.ID = "acme_corp" }},
		{"leading dash in id", "id", func(c *customer.Config) { c.ID = "-acme" }},
		{"single char id", "id", func(c *customer.Config) { c.ID = "a" }},
		{"empty tier", "tier", func(c *customer.Config) { c.Tier = "" }},
		{"unknown tier", "tier", func(c *customer.Config) { c.Tier = "platinum" }},
		{"empty region", "region", func(c *customer.Config) { c.Region = "" }},
		{"malformed region", "region", func(c *customer.Config) { c.Region = "europe" }},
		{"region without index", "region", func(c *customer.Config) { c.Region = "eu-west" }},
		{"unknown feature", "features", func(c *customer.Config) { c.Features = []string{"teleportation"} }},
		{"duplicate feature", "features", func(c *customer.Config) {
			c.Features = []string{"mesh", "mesh"}
		}},
		{"negative replicas", "replicas", func(c *customer.Config) { c.Replicas = -1 }},
		{"absurd replicas", "replicas", func(c *customer.Config) { c.Replicas = 100000 }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid()
			tc.mutate(&cfg)
			problems := cfg.Valid(known)
			if _, ok := problems[tc.field]; !ok {
				t.Errorf("expected a problem for %q, got %v", tc.field, problems)
			}
		})
	}
}

// The problem map is what a bulk caller reads to fix 4,000 configs, so the
// explanations have to name what was wrong, not just that something was.
func TestProblemsExplainThemselves(t *testing.T) {
	cfg := valid()
	cfg.Tier = "platinum"
	cfg.Features = []string{"teleportation"}

	problems := cfg.Valid(known)

	if !strings.Contains(problems["tier"], "platinum") {
		t.Errorf("tier problem should quote the bad value: %q", problems["tier"])
	}
	if !strings.Contains(problems["tier"], "premium") {
		t.Errorf("tier problem should list the valid options: %q", problems["tier"])
	}
	if !strings.Contains(problems["features"], "teleportation") {
		t.Errorf("features problem should name the unknown feature: %q", problems["features"])
	}
}

// Limits are the only thing standing between a bulk endpoint and an
// out-of-memory kill, so they get their own tests.
func TestOverrideLimits(t *testing.T) {
	t.Run("depth", func(t *testing.T) {
		cfg := valid()
		deep := map[string]any{"leaf": "value"}
		for i := 0; i < customer.MaxOverrideDepth+2; i++ {
			deep = map[string]any{"nested": deep}
		}
		cfg.Overrides = map[string]map[string]any{"rel": deep}

		problems := cfg.Valid(known)
		if _, ok := problems["overrides.rel"]; !ok {
			t.Errorf("expected a depth problem, got %v", problems)
		}
	})

	t.Run("key count", func(t *testing.T) {
		cfg := valid()
		wide := make(map[string]any, customer.MaxOverrideKeys+10)
		for i := 0; i < customer.MaxOverrideKeys+10; i++ {
			wide["key"+strconv.Itoa(i)] = i
		}
		cfg.Overrides = map[string]map[string]any{"rel": wide}

		if _, ok := cfg.Valid(known)["overrides"]; !ok {
			t.Error("expected a key-count problem")
		}
	})

	t.Run("acceptable override passes", func(t *testing.T) {
		cfg := valid()
		cfg.Overrides = map[string]map[string]any{
			"rel": {"image": map[string]any{"tag": "2.0.0"}, "replicaCount": 5},
		}
		if problems := cfg.Valid(known); len(problems) > 0 {
			t.Errorf("expected a reasonable override to pass, got %v", problems)
		}
	})

	t.Run("too many features", func(t *testing.T) {
		cfg := valid()
		cfg.Features = make([]string, customer.MaxFeatures+1)
		for i := range cfg.Features {
			cfg.Features[i] = "observability"
		}
		if _, ok := cfg.Valid(known)["features"]; !ok {
			t.Error("expected a feature-count problem")
		}
	})
}

// With no blueprint loaded there is nothing to check feature names against,
// so unknown-feature checking is skipped rather than rejecting everything.
func TestUnknownFeaturesSkippedWithoutABlueprint(t *testing.T) {
	cfg := valid()
	cfg.Features = []string{"anything-at-all"}
	if problems := cfg.Valid(nil); len(problems) > 0 {
		t.Errorf("expected no problems without a known-feature set, got %v", problems)
	}
}
