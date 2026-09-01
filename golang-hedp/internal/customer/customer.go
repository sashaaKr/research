// Package customer defines the input the service validates and renders.
//
// Validation runs before any rendering, and it is not decoration: rendering a
// 55 MB chart library to discover that a tier name was misspelled is the
// expensive way to find out. Every check here is one the renderer would
// otherwise pay for in template execution time.
package customer

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Config is one customer's configuration. A bulk request carries thousands.
type Config struct {
	ID       string   `json:"id"`
	Tier     string   `json:"tier"`
	Region   string   `json:"region"`
	Features []string `json:"features,omitempty"`
	Replicas int      `json:"replicas,omitempty"`
	// Overrides are per-release value overrides, keyed by release name. They
	// are merged last, over both the blueprint's values and the derived ones.
	Overrides map[string]map[string]any `json:"overrides,omitempty"`
	// RenderAll ignores feature gates and renders the entire library. This is
	// the "does everything still compile" path, and it is orders of magnitude
	// more expensive than a normal request - see the benchmarks.
	RenderAll bool `json:"render_all,omitempty"`
}

// Tiers are the accepted values for Config.Tier.
var Tiers = []string{"free", "standard", "premium", "enterprise"}

var (
	idPattern     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)
	regionPattern = regexp.MustCompile(`^[a-z]{2}-[a-z]+-[1-9][0-9]?$`)
)

// Limits bound how much work a single config may ask for. Without them a
// bulk request is an unauthenticated way to allocate arbitrary memory.
const (
	MaxFeatures      = 64
	MaxReplicas      = 1000
	MaxOverrideDepth = 8
	MaxOverrideKeys  = 512
)

// Valid reports problems as field -> human-readable explanation. An empty map
// means the config is renderable.
//
// Returning a map rather than a bare error is what lets a bulk response tell a
// caller which of their 4,000 configs failed and in which field.
func (c Config) Valid(knownFeatures map[string]bool) map[string]string {
	problems := map[string]string{}

	switch {
	case c.ID == "":
		problems["id"] = "is required"
	case !idPattern.MatchString(c.ID):
		problems["id"] = "must be 2-63 chars of [a-z0-9-] and start with a letter or digit"
	}

	if c.Tier == "" {
		problems["tier"] = "is required, one of " + strings.Join(Tiers, ", ")
	} else if !contains(Tiers, c.Tier) {
		problems["tier"] = fmt.Sprintf("%q is not a known tier, want one of %s", c.Tier, strings.Join(Tiers, ", "))
	}

	switch {
	case c.Region == "":
		problems["region"] = "is required"
	case !regionPattern.MatchString(c.Region):
		problems["region"] = fmt.Sprintf("%q is not a region identifier, want e.g. eu-west-1", c.Region)
	}

	if len(c.Features) > MaxFeatures {
		problems["features"] = fmt.Sprintf("%d features exceeds the limit of %d", len(c.Features), MaxFeatures)
	} else if len(knownFeatures) > 0 {
		var unknown []string
		seen := map[string]bool{}
		for _, f := range c.Features {
			if seen[f] {
				problems["features"] = fmt.Sprintf("%q is listed more than once", f)
				continue
			}
			seen[f] = true
			if !knownFeatures[f] {
				unknown = append(unknown, f)
			}
		}
		if len(unknown) > 0 {
			sort.Strings(unknown)
			problems["features"] = fmt.Sprintf("unknown: %s", strings.Join(unknown, ", "))
		}
	}

	if c.Replicas < 0 || c.Replicas > MaxReplicas {
		problems["replicas"] = fmt.Sprintf("must be between 0 and %d", MaxReplicas)
	}

	keys := 0
	for release, override := range c.Overrides {
		if release == "" {
			problems["overrides"] = "release names must not be empty"
			break
		}
		d, k := shape(override, 1)
		keys += k
		if d > MaxOverrideDepth {
			problems["overrides."+release] = fmt.Sprintf("nested %d deep, limit is %d", d, MaxOverrideDepth)
		}
	}
	if keys > MaxOverrideKeys {
		problems["overrides"] = fmt.Sprintf("%d total keys exceeds the limit of %d", keys, MaxOverrideKeys)
	}

	return problems
}

// shape returns the depth and total key count of an arbitrary decoded JSON
// object. Both are bounded before the value is ever handed to the template
// engine: deeply nested values are cheap to send and expensive to coalesce.
func shape(v any, depth int) (maxDepth, keys int) {
	maxDepth = depth
	switch t := v.(type) {
	case map[string]any:
		for _, child := range t {
			keys++
			d, k := shape(child, depth+1)
			keys += k
			if d > maxDepth {
				maxDepth = d
			}
		}
	case []any:
		for _, child := range t {
			d, k := shape(child, depth+1)
			keys += k
			if d > maxDepth {
				maxDepth = d
			}
		}
	}
	return maxDepth, keys
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
