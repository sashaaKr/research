//! Chart loading, mirroring the Go service's internal/catalog.
//!
//! Same contract: read everything once at startup, hold it immutably, share it
//! across every request. The one structural difference is that templates are
//! compiled into a MiniJinja Environment at load time rather than being kept
//! as source - which is the Rust equivalent of the Go port's cached engine,
//! and the only fair thing to compare against it.

use std::collections::BTreeMap;
use std::path::Path;
use std::sync::Arc;
use std::time::Instant;

use anyhow::{anyhow, Context, Result};
use minijinja::Environment;
use rayon::prelude::*;
use serde::Serialize;

use crate::filters;

/// Chart is one immutable chart: its compiled templates, its non-template
/// files, and its default values.
pub struct Chart {
    pub name: String,
    /// env holds every template of this chart, already parsed. Rendering only
    /// executes.
    pub env: Environment<'static>,
    /// render_order is the template execution order, matching Helm's: deepest
    /// paths first, so a top-level definition wins over a nested one.
    pub render_order: Vec<String>,
    /// files are the chart's non-template payload, reachable from templates.
    pub files: BTreeMap<String, String>,
    pub values: serde_json::Value,
    pub app_version: String,
    pub version: String,

    pub template_bytes: u64,
    pub file_bytes: u64,
    pub crd_bytes: u64,
}

#[derive(Default, Clone, Serialize)]
pub struct Stats {
    pub charts: usize,
    pub template_files: usize,
    pub template_bytes: u64,
    pub file_files: usize,
    pub file_bytes: u64,
    pub crd_files: usize,
    pub crd_bytes: u64,
    pub total_bytes: u64,
    pub load_millis: u64,
    pub compile_millis: u64,
}

pub struct Catalog {
    charts: BTreeMap<String, Arc<Chart>>,
    stats: Stats,
}

impl Catalog {
    pub fn chart(&self, name: &str) -> Option<&Arc<Chart>> {
        self.charts.get(name)
    }

    pub fn names(&self) -> Vec<String> {
        self.charts.keys().cloned().collect()
    }

    pub fn stats(&self) -> &Stats {
        &self.stats
    }

    /// load reads every chart directory under `root`, in parallel.
    pub fn load(root: &Path) -> Result<Self> {
        let start = Instant::now();

        let mut dirs: Vec<_> = std::fs::read_dir(root)
            .with_context(|| format!("read chart root {}", root.display()))?
            .filter_map(|e| e.ok())
            .filter(|e| e.path().is_dir())
            .map(|e| e.path())
            .collect();
        dirs.sort();
        if dirs.is_empty() {
            return Err(anyhow!("no charts found under {}", root.display()));
        }

        let loaded: Result<Vec<Chart>> = dirs.par_iter().map(|d| load_chart(d)).collect();
        let loaded = loaded?;

        let mut stats = Stats::default();
        let mut charts = BTreeMap::new();
        for c in loaded {
            stats.charts += 1;
            stats.template_files += c.render_order.len();
            stats.template_bytes += c.template_bytes;
            stats.file_files += c.files.len();
            stats.file_bytes += c.file_bytes;
            stats.crd_bytes += c.crd_bytes;
            charts.insert(c.name.clone(), Arc::new(c));
        }
        stats.total_bytes = stats.template_bytes + stats.file_bytes + stats.crd_bytes;
        stats.load_millis = start.elapsed().as_millis() as u64;

        Ok(Catalog { charts, stats })
    }
}

fn load_chart(dir: &Path) -> Result<Chart> {
    let name = dir
        .file_name()
        .and_then(|s| s.to_str())
        .ok_or_else(|| anyhow!("bad chart dir {}", dir.display()))?
        .to_string();

    let chart_yaml = std::fs::read_to_string(dir.join("Chart.yaml"))
        .with_context(|| format!("read Chart.yaml for {name}"))?;
    let meta: serde_yaml_ng::Value = serde_yaml_ng::from_str(&chart_yaml)?;
    let app_version = meta
        .get("appVersion")
        .and_then(|v| v.as_str())
        .unwrap_or("")
        .to_string();
    let version = meta
        .get("version")
        .and_then(|v| v.as_str())
        .unwrap_or("")
        .to_string();

    let values_yaml = std::fs::read_to_string(dir.join("values.yaml"))
        .with_context(|| format!("read values.yaml for {name}"))?;
    let values_y: serde_yaml_ng::Value = serde_yaml_ng::from_str(&values_yaml)?;
    let values: serde_json::Value = serde_json::to_value(values_y)?;

    let mut templates: Vec<(String, String)> = Vec::new();
    let mut files: BTreeMap<String, String> = BTreeMap::new();
    let mut template_bytes = 0u64;
    let mut file_bytes = 0u64;
    let mut crd_bytes = 0u64;

    walk(dir, dir, &mut |rel: String, body: Vec<u8>| {
        if rel.starts_with("templates/") {
            template_bytes += body.len() as u64;
            templates.push((rel, String::from_utf8_lossy(&body).into_owned()));
        } else if rel.starts_with("crds/") {
            // Never parsed, never read - counted only so the byte split lines
            // up with the Go service's report.
            crd_bytes += body.len() as u64;
        } else if rel != "Chart.yaml" && rel != "values.yaml" {
            file_bytes += body.len() as u64;
            files.insert(rel, String::from_utf8_lossy(&body).into_owned());
        }
    })?;

    // Deepest paths first, then reverse-lexicographic - the same comparator
    // Helm's sortTemplates uses, so a redefinition resolves the same way.
    templates.sort_by(|a, b| {
        let (ca, cb) = (a.0.matches('/').count(), b.0.matches('/').count());
        if ca == cb {
            b.0.cmp(&a.0)
        } else {
            cb.cmp(&ca)
        }
    });

    let mut env = Environment::new();
    filters::register(&mut env);
    // Jinja strips a template's final newline by default; Go's text/template
    // does not. Keeping it is what makes the two engines' output identical.
    env.set_keep_trailing_newline(true);

    let mut render_order = Vec::with_capacity(templates.len());
    for (path, source) in templates {
        // MiniJinja resolves `import` by template name, so register under the
        // basename the templates import each other by.
        let key = path
            .strip_prefix("templates/")
            .unwrap_or(&path)
            .to_string();
        env.add_template_owned(key.clone(), source)
            .with_context(|| format!("compile {name}/{path}"))?;

        // Partials are only reached through import; their direct output is not
        // a manifest.
        let base = key.rsplit('/').next().unwrap_or(&key);
        if !base.starts_with('_') {
            render_order.push(key);
        }
    }

    Ok(Chart {
        name,
        env,
        render_order,
        files,
        values,
        app_version,
        version,
        template_bytes,
        file_bytes,
        crd_bytes,
    })
}

fn walk(root: &Path, dir: &Path, f: &mut impl FnMut(String, Vec<u8>)) -> Result<()> {
    for entry in std::fs::read_dir(dir)? {
        let entry = entry?;
        let path = entry.path();
        if path.is_dir() {
            walk(root, &path, f)?;
            continue;
        }
        let rel = path
            .strip_prefix(root)?
            .to_string_lossy()
            .replace('\\', "/");
        let body = std::fs::read(&path)?;
        f(rel, body);
    }
    Ok(())
}
