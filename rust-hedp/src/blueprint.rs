//! The release graph, ported from the Go service's internal/blueprint.
//!
//! Same semantics throughout: feature gates select releases, dependency edges
//! pull in whatever those need, cycles are rejected at load, and the selection
//! is partitioned into waves by longest dependency chain.

use std::collections::{BTreeMap, BTreeSet, HashMap};

use anyhow::{anyhow, Context, Result};
use serde::{Deserialize, Serialize};

#[derive(Debug, Clone, Deserialize, Serialize)]
pub struct Release {
    pub name: String,
    pub chart: String,
    #[serde(default)]
    pub namespace: String,
    #[serde(default)]
    pub requires: Vec<String>,
    #[serde(default, rename = "dependsOn")]
    pub depends_on: Vec<String>,
    #[serde(default)]
    pub values: serde_json::Value,
    #[serde(default)]
    pub exports: BTreeMap<String, String>,
}

#[derive(Debug, Deserialize)]
struct BlueprintFile {
    releases: Vec<Release>,
}

pub struct Blueprint {
    pub releases: Vec<Release>,
    by_name: HashMap<String, usize>,
    /// order is a global topological order; any subset of it is valid for that
    /// subset.
    order: HashMap<String, usize>,
}

#[derive(Debug, Default, Serialize)]
pub struct Plan {
    pub waves: Vec<Vec<String>>,
    pub selected: Vec<String>,
    pub pulled_in: Vec<String>,
}

impl Plan {
    pub fn count(&self) -> usize {
        self.selected.len()
    }
    pub fn width(&self) -> usize {
        self.waves.iter().map(|w| w.len()).max().unwrap_or(0)
    }
}

impl Blueprint {
    pub fn load(path: &std::path::Path) -> Result<Self> {
        let data = std::fs::read_to_string(path)
            .with_context(|| format!("read blueprint {}", path.display()))?;
        Self::parse(&data)
    }

    pub fn parse(data: &str) -> Result<Self> {
        let file: BlueprintFile = serde_yaml_ng::from_str(data).context("parse blueprint")?;
        if file.releases.is_empty() {
            return Err(anyhow!("blueprint has no releases"));
        }

        let mut releases = file.releases;
        let mut by_name = HashMap::with_capacity(releases.len());
        for (i, r) in releases.iter_mut().enumerate() {
            if r.name.is_empty() {
                return Err(anyhow!("release {i} has no name"));
            }
            if r.chart.is_empty() {
                return Err(anyhow!("release {} has no chart", r.name));
            }
            if r.namespace.is_empty() {
                r.namespace = "default".into();
            }
            if by_name.insert(r.name.clone(), i).is_some() {
                return Err(anyhow!("duplicate release {}", r.name));
            }
        }
        for r in &releases {
            for dep in &r.depends_on {
                if !by_name.contains_key(dep) {
                    return Err(anyhow!("release {} depends on unknown release {dep}", r.name));
                }
            }
        }

        let order = topo_sort(&releases, &by_name)?;
        Ok(Blueprint {
            releases,
            by_name,
            order,
        })
    }

    pub fn get(&self, name: &str) -> Option<&Release> {
        self.by_name.get(name).map(|i| &self.releases[*i])
    }

    pub fn names(&self) -> Vec<String> {
        let mut n: Vec<String> = self.releases.iter().map(|r| r.name.clone()).collect();
        n.sort_by_key(|name| self.order[name]);
        n
    }

    pub fn features(&self) -> Vec<String> {
        let set: BTreeSet<&String> = self.releases.iter().flat_map(|r| r.requires.iter()).collect();
        set.into_iter().cloned().collect()
    }

    pub fn validate(&self, has_chart: impl Fn(&str) -> bool) -> Result<()> {
        let missing: Vec<String> = self
            .releases
            .iter()
            .filter(|r| !has_chart(&r.chart))
            .map(|r| format!("{} -> {}", r.name, r.chart))
            .collect();
        if !missing.is_empty() {
            return Err(anyhow!(
                "blueprint references {} missing charts: {}",
                missing.len(),
                missing.join(", ")
            ));
        }
        Ok(())
    }

    /// plan_for selects what a customer's features imply and partitions it into
    /// dependency waves.
    pub fn plan_for(&self, features: &[String], all: bool) -> Plan {
        let enabled: BTreeSet<&str> = features.iter().map(|s| s.as_str()).collect();

        let mut direct = BTreeSet::new();
        for r in &self.releases {
            if all || r.requires.iter().all(|f| enabled.contains(f.as_str())) {
                direct.insert(r.name.clone());
            }
        }

        let mut selected: BTreeSet<String> = BTreeSet::new();
        let mut stack: Vec<String> = direct.iter().cloned().collect();
        while let Some(name) = stack.pop() {
            if !selected.insert(name.clone()) {
                continue;
            }
            if let Some(r) = self.get(&name) {
                for d in &r.depends_on {
                    if !selected.contains(d) {
                        stack.push(d.clone());
                    }
                }
            }
        }

        let mut flat: Vec<String> = selected.iter().cloned().collect();
        flat.sort_by_key(|n| self.order[n]);

        let pulled_in: Vec<String> = flat
            .iter()
            .filter(|n| !direct.contains(*n))
            .cloned()
            .collect();

        // Depth is the longest dependency chain inside the selection, i.e. the
        // earliest wave a release can run in.
        let mut depth: HashMap<&str, usize> = HashMap::with_capacity(flat.len());
        let mut max_depth = 0;
        for name in &flat {
            let mut d = 0;
            if let Some(r) = self.get(name) {
                for dep in &r.depends_on {
                    if selected.contains(dep) {
                        d = d.max(depth.get(dep.as_str()).copied().unwrap_or(0) + 1);
                    }
                }
            }
            depth.insert(name.as_str(), d);
            max_depth = max_depth.max(d);
        }
        let mut waves = vec![Vec::new(); max_depth + 1];
        for name in &flat {
            waves[depth[name.as_str()]].push(name.clone());
        }

        Plan {
            waves,
            selected: flat,
            pulled_in,
        }
    }
}

fn topo_sort(
    releases: &[Release],
    by_name: &HashMap<String, usize>,
) -> Result<HashMap<String, usize>> {
    const WHITE: u8 = 0;
    const GREY: u8 = 1;
    const BLACK: u8 = 2;

    let mut state: HashMap<&str, u8> = HashMap::with_capacity(releases.len());
    let mut out: Vec<&str> = Vec::with_capacity(releases.len());

    let mut names: Vec<&str> = releases.iter().map(|r| r.name.as_str()).collect();
    names.sort();

    // Iterative DFS: a deep chain should not be able to blow the stack.
    for root in names {
        if state.get(root).copied().unwrap_or(WHITE) == BLACK {
            continue;
        }
        let mut stack: Vec<(&str, usize)> = vec![(root, 0)];
        while let Some((name, idx)) = stack.pop() {
            if idx == 0 {
                match state.get(name).copied().unwrap_or(WHITE) {
                    BLACK => continue,
                    GREY => {
                        let path: Vec<&str> = stack.iter().map(|(n, _)| *n).collect();
                        return Err(anyhow!("dependency cycle: {} -> {name}", path.join(" -> ")));
                    }
                    _ => {}
                }
                state.insert(name, GREY);
            }

            let deps = {
                let r = &releases[by_name[name]];
                let mut d: Vec<&str> = r.depends_on.iter().map(|s| s.as_str()).collect();
                d.sort();
                d
            };

            if idx < deps.len() {
                stack.push((name, idx + 1));
                let next = deps[idx];
                if state.get(next).copied().unwrap_or(WHITE) == GREY {
                    return Err(anyhow!("dependency cycle: {name} -> {next}"));
                }
                stack.push((next, 0));
            } else {
                state.insert(name, BLACK);
                out.push(name);
            }
        }
    }

    Ok(out
        .into_iter()
        .enumerate()
        .map(|(i, n)| (n.to_string(), i))
        .collect())
}
