//! The input the service validates and renders, ported from internal/customer.
//!
//! Same rules, same limits, same field -> explanation problem map, so a
//! rejected config costs the same on both sides and the benchmark is not
//! quietly comparing different amounts of validation.

use std::collections::BTreeMap;
use std::sync::LazyLock;

use regex::Regex;
use serde::{Deserialize, Serialize};

pub const TIERS: [&str; 4] = ["free", "standard", "premium", "enterprise"];

pub const MAX_FEATURES: usize = 64;
pub const MAX_REPLICAS: i64 = 1000;
pub const MAX_OVERRIDE_DEPTH: usize = 8;
pub const MAX_OVERRIDE_KEYS: usize = 512;

static ID_PATTERN: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"^[a-z0-9][a-z0-9-]{1,62}$").unwrap());
static REGION_PATTERN: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"^[a-z]{2}-[a-z]+-[1-9][0-9]?$").unwrap());

#[derive(Debug, Clone, Default, Deserialize, Serialize)]
pub struct Config {
    pub id: String,
    pub tier: String,
    pub region: String,
    #[serde(default)]
    pub features: Vec<String>,
    #[serde(default)]
    pub replicas: i64,
    #[serde(default)]
    pub overrides: BTreeMap<String, serde_json::Value>,
    #[serde(default)]
    pub render_all: bool,
}

impl Config {
    /// validate reports problems as field -> human-readable explanation. An
    /// empty map means the config is renderable.
    pub fn validate(&self, known_features: &[String]) -> BTreeMap<String, String> {
        let mut problems = BTreeMap::new();

        if self.id.is_empty() {
            problems.insert("id".into(), "is required".into());
        } else if !ID_PATTERN.is_match(&self.id) {
            problems.insert(
                "id".into(),
                "must be 2-63 chars of [a-z0-9-] and start with a letter or digit".into(),
            );
        }

        if self.tier.is_empty() {
            problems.insert(
                "tier".into(),
                format!("is required, one of {}", TIERS.join(", ")),
            );
        } else if !TIERS.contains(&self.tier.as_str()) {
            problems.insert(
                "tier".into(),
                format!(
                    "\"{}\" is not a known tier, want one of {}",
                    self.tier,
                    TIERS.join(", ")
                ),
            );
        }

        if self.region.is_empty() {
            problems.insert("region".into(), "is required".into());
        } else if !REGION_PATTERN.is_match(&self.region) {
            problems.insert(
                "region".into(),
                format!(
                    "\"{}\" is not a region identifier, want e.g. eu-west-1",
                    self.region
                ),
            );
        }

        if self.features.len() > MAX_FEATURES {
            problems.insert(
                "features".into(),
                format!(
                    "{} features exceeds the limit of {}",
                    self.features.len(),
                    MAX_FEATURES
                ),
            );
        } else if !known_features.is_empty() {
            let mut seen = std::collections::BTreeSet::new();
            let mut unknown = Vec::new();
            for f in &self.features {
                if !seen.insert(f) {
                    problems.insert("features".into(), format!("\"{f}\" is listed more than once"));
                    continue;
                }
                if !known_features.contains(f) {
                    unknown.push(f.clone());
                }
            }
            if !unknown.is_empty() {
                unknown.sort();
                problems.insert("features".into(), format!("unknown: {}", unknown.join(", ")));
            }
        }

        if self.replicas < 0 || self.replicas > MAX_REPLICAS {
            problems.insert(
                "replicas".into(),
                format!("must be between 0 and {MAX_REPLICAS}"),
            );
        }

        let mut keys = 0usize;
        for (release, override_val) in &self.overrides {
            if release.is_empty() {
                problems.insert("overrides".into(), "release names must not be empty".into());
                break;
            }
            let (depth, k) = shape(override_val, 1);
            keys += k;
            if depth > MAX_OVERRIDE_DEPTH {
                problems.insert(
                    format!("overrides.{release}"),
                    format!("nested {depth} deep, limit is {MAX_OVERRIDE_DEPTH}"),
                );
            }
        }
        if keys > MAX_OVERRIDE_KEYS {
            problems.insert(
                "overrides".into(),
                format!("{keys} total keys exceeds the limit of {MAX_OVERRIDE_KEYS}"),
            );
        }

        problems
    }
}

/// shape returns depth and total key count. Both are bounded before the value
/// reaches the template engine: deep values are cheap to send and expensive to
/// merge.
fn shape(v: &serde_json::Value, depth: usize) -> (usize, usize) {
    let mut max_depth = depth;
    let mut keys = 0;
    match v {
        serde_json::Value::Object(m) => {
            for child in m.values() {
                keys += 1;
                let (d, k) = shape(child, depth + 1);
                keys += k;
                max_depth = max_depth.max(d);
            }
        }
        serde_json::Value::Array(a) => {
            for child in a {
                let (d, k) = shape(child, depth + 1);
                keys += k;
                max_depth = max_depth.max(d);
            }
        }
        _ => {}
    }
    (max_depth, keys)
}
