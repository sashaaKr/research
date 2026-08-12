//! The render pipeline, ported from the Go service's internal/render.
//!
//! Deliberately the same shape, because a benchmark between two programs that
//! schedule work differently measures the scheduling, not the language: select
//! releases, order them into dependency waves, render each wave, feed exports
//! to dependents, report a per-release verdict with a content digest.

use std::collections::BTreeMap;
use std::sync::Arc;
use std::time::Instant;

use anyhow::Result;
use minijinja::value::Value as JinjaValue;
use rayon::prelude::*;
use serde::Serialize;
use serde_json::{json, Map, Value};
use sha2::{Digest, Sha256};

use crate::blueprint::{Blueprint, Release};
use crate::catalog::{Catalog, Chart};
use crate::customer::Config;

pub struct Options {
    /// Parallel releases within one customer. 1 is the bulk-tuned setting: the
    /// cores are already busy with other customers.
    pub release_concurrency: usize,
    /// Return rendered manifests. Off by default - bulk validation wants a
    /// verdict, and the payloads for a 55 MB library dwarf it.
    pub include_manifests: bool,
}

impl Default for Options {
    fn default() -> Self {
        Options {
            release_concurrency: 1,
            include_manifests: false,
        }
    }
}

#[derive(Debug, Default, Clone, Serialize)]
pub struct ReleaseResult {
    pub name: String,
    pub chart: String,
    pub namespace: String,
    pub manifests: usize,
    pub bytes: usize,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub digest: String,
    pub micros: u64,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub error: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub skipped: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub files: Option<BTreeMap<String, String>>,
}

#[derive(Debug, Default, Clone, Serialize)]
pub struct RenderResult {
    pub customer_id: String,
    pub ok: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub problems: Option<BTreeMap<String, String>>,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub releases: Vec<ReleaseResult>,
    pub planned: usize,
    pub pulled_in: usize,
    pub waves: usize,
    pub manifests: usize,
    pub total_bytes: usize,
    pub micros: u64,
    pub failed: usize,
}

pub struct Renderer {
    catalog: Arc<Catalog>,
    blueprint: Arc<Blueprint>,
    opts: Options,
    features: Vec<String>,
    /// Export templates, compiled once at startup rather than per render. They
    /// are tiny, but "tiny times thousands of customers times dozens of
    /// releases" is not.
    exports: BTreeMap<String, minijinja::Environment<'static>>,
}

impl Renderer {
    pub fn new(catalog: Arc<Catalog>, blueprint: Arc<Blueprint>, opts: Options) -> Result<Self> {
        blueprint.validate(|name| catalog.chart(name).is_some())?;

        let mut exports = BTreeMap::new();
        for r in &blueprint.releases {
            if r.exports.is_empty() {
                continue;
            }
            let mut env = minijinja::Environment::new();
            crate::filters::register(&mut env);
            for (key, tpl) in &r.exports {
                // Blueprint exports are written in Go template syntax so the
                // two services can share one blueprint file; translate the two
                // constructs they actually use.
                let converted = tpl
                    .replace("{{ .Release.Name }}", "{{ release.name }}")
                    .replace("{{ .Release.Namespace }}", "{{ release.namespace }}");
                env.add_template_owned(key.clone(), converted)?;
            }
            exports.insert(r.name.clone(), env);
        }

        let features = blueprint.features();
        Ok(Renderer {
            catalog,
            blueprint,
            opts,
            features,
            exports,
        })
    }

    pub fn features(&self) -> &[String] {
        &self.features
    }

    pub fn blueprint(&self) -> &Blueprint {
        &self.blueprint
    }

    pub fn catalog(&self) -> &Catalog {
        &self.catalog
    }

    /// render validates and renders one customer configuration.
    pub fn render(&self, cfg: &Config) -> RenderResult {
        let start = Instant::now();
        let mut res = RenderResult {
            customer_id: cfg.id.clone(),
            ..Default::default()
        };

        let problems = cfg.validate(&self.features);
        if !problems.is_empty() {
            res.problems = Some(problems);
            res.micros = start.elapsed().as_micros() as u64;
            return res;
        }

        let plan = self.blueprint.plan_for(&cfg.features, cfg.render_all);
        res.planned = plan.count();
        res.pulled_in = plan.pulled_in.len();
        res.waves = plan.waves.len();

        let mut exports: BTreeMap<String, BTreeMap<String, String>> = BTreeMap::new();
        let mut failed: BTreeMap<String, String> = BTreeMap::new();
        let mut results: Vec<ReleaseResult> = Vec::with_capacity(plan.count());

        for wave in &plan.waves {
            // A release whose upstream failed is not rendered: its values would
            // be missing the exports it was written against, so the errors
            // would be noise. Report the cause instead.
            let mut runnable = Vec::with_capacity(wave.len());
            for name in wave {
                let rel = match self.blueprint.get(name) {
                    Some(r) => r,
                    None => continue,
                };
                if let Some(blocked) = rel.depends_on.iter().find(|d| failed.contains_key(*d)) {
                    results.push(ReleaseResult {
                        name: name.clone(),
                        chart: rel.chart.clone(),
                        namespace: rel.namespace.clone(),
                        skipped: format!("dependency {blocked} failed"),
                        ..Default::default()
                    });
                    failed.insert(name.clone(), blocked.clone());
                    continue;
                }
                runnable.push(rel);
            }

            let rendered: Vec<(ReleaseResult, BTreeMap<String, String>)> =
                if self.opts.release_concurrency > 1 {
                    runnable
                        .par_iter()
                        .map(|rel| self.render_release(rel, cfg, &exports))
                        .collect()
                } else {
                    runnable
                        .iter()
                        .map(|rel| self.render_release(rel, cfg, &exports))
                        .collect()
                };

            for (rr, exp) in rendered {
                if rr.error.is_empty() {
                    exports.insert(rr.name.clone(), exp);
                } else {
                    failed.insert(rr.name.clone(), rr.name.clone());
                }
                results.push(rr);
            }
        }

        results.sort_by(|a, b| a.name.cmp(&b.name));
        for rr in &results {
            res.manifests += rr.manifests;
            res.total_bytes += rr.bytes;
            if !rr.error.is_empty() || !rr.skipped.is_empty() {
                res.failed += 1;
            }
        }
        res.ok = res.failed == 0;
        res.releases = results;
        res.micros = start.elapsed().as_micros() as u64;
        res
    }

    /// render_release is the hot path. Everything else in this file exists to
    /// decide how often and in what order it runs.
    fn render_release(
        &self,
        rel: &Release,
        cfg: &Config,
        upstream: &BTreeMap<String, BTreeMap<String, String>>,
    ) -> (ReleaseResult, BTreeMap<String, String>) {
        let start = Instant::now();
        let mut out = ReleaseResult {
            name: rel.name.clone(),
            chart: rel.chart.clone(),
            namespace: rel.namespace.clone(),
            ..Default::default()
        };

        let chart = match self.catalog.chart(&rel.chart) {
            Some(c) => c,
            None => {
                out.error = format!("chart not found: {}", rel.chart);
                return (out, BTreeMap::new());
            }
        };

        let values = self.build_values(chart, rel, cfg, upstream);
        let release_name = format!("{}-{}", cfg.id, rel.name);

        let ctx = json!({
            "Chart": {
                "Name": chart.name,
                "Version": chart.version,
                "AppVersion": chart.app_version,
            },
            "Release": {
                "Name": release_name,
                "Namespace": rel.namespace,
                "Service": "Helm",
                "IsInstall": true,
                "Revision": 1,
            },
            "Values": values,
        });
        let ctx_value = JinjaValue::from_serialize(&ctx);
        let files_value = JinjaValue::from_serialize(&chart.files);

        let mut names: Vec<&str> = Vec::with_capacity(chart.render_order.len());
        let mut bodies: Vec<String> = Vec::with_capacity(chart.render_order.len());

        for key in &chart.render_order {
            let tmpl = match chart.env.get_template(key) {
                Ok(t) => t,
                Err(e) => {
                    out.error = trim_err(&e.to_string());
                    out.micros = start.elapsed().as_micros() as u64;
                    return (out, BTreeMap::new());
                }
            };
            let body = match tmpl.render(minijinja::context! {
                ctx => ctx_value.clone(),
                files => files_value.clone(),
            }) {
                Ok(b) => b,
                Err(e) => {
                    out.error = trim_err(&e.to_string());
                    out.micros = start.elapsed().as_micros() as u64;
                    return (out, BTreeMap::new());
                }
            };
            // Whitespace-only output is how a gated template says "not for this
            // customer"; counting it would make every plan look identical.
            if body.trim().is_empty() {
                continue;
            }
            names.push(key);
            bodies.push(body);
        }

        let mut hasher = Sha256::new();
        for (name, body) in names.iter().zip(bodies.iter()) {
            out.bytes += body.len();
            hasher.update(name.as_bytes());
            hasher.update(body.as_bytes());
        }
        out.manifests = names.len();
        out.digest = hex::encode(hasher.finalize())[..16].to_string();

        if self.opts.include_manifests {
            out.files = Some(
                names
                    .iter()
                    .zip(bodies.iter())
                    .map(|(n, b)| (n.to_string(), b.clone()))
                    .collect(),
            );
        }
        out.micros = start.elapsed().as_micros() as u64;

        let exports = self.eval_exports(rel, &release_name);
        (out, exports)
    }

    /// build_values layers the value sources. Order is the contract: chart
    /// defaults, then blueprint values, then values derived from the customer,
    /// then upstream exports, then the customer's overrides last so they win.
    fn build_values(
        &self,
        chart: &Chart,
        rel: &Release,
        cfg: &Config,
        upstream: &BTreeMap<String, BTreeMap<String, String>>,
    ) -> Value {
        let mut values = chart.values.clone();
        merge_into(&mut values, &rel.values);

        let obj = values.as_object_mut().expect("chart values must be a map");
        obj.insert(
            "customer".into(),
            json!({
                "id": cfg.id,
                "tier": cfg.tier,
                "region": cfg.region,
                "features": cfg.features,
            }),
        );
        if cfg.replicas > 0 {
            obj.insert("replicaCount".into(), json!(cfg.replicas));
        }
        if !rel.depends_on.is_empty() {
            let mut deps = Map::new();
            for d in &rel.depends_on {
                if let Some(exp) = upstream.get(d) {
                    deps.insert(d.clone(), serde_json::to_value(exp).unwrap_or(Value::Null));
                }
            }
            if !deps.is_empty() {
                obj.insert("deps".into(), Value::Object(deps));
            }
        }
        if let Some(over) = cfg.overrides.get(&rel.name) {
            merge_into(&mut values, &serde_json::to_value(over).unwrap_or(Value::Null));
        }
        values
    }

    fn eval_exports(&self, rel: &Release, release_name: &str) -> BTreeMap<String, String> {
        let env = match self.exports.get(&rel.name) {
            Some(e) => e,
            None => return BTreeMap::new(),
        };
        let ctx = minijinja::context! {
            release => minijinja::context! {
                name => release_name,
                namespace => rel.namespace.clone(),
            },
        };
        let mut out = BTreeMap::new();
        for key in rel.exports.keys() {
            if let Ok(t) = env.get_template(key) {
                out.insert(key.clone(), t.render(&ctx).unwrap_or_default());
            }
        }
        out
    }
}

/// merge_into deep-merges src into dst: maps merge, everything else replaces.
/// Mirrors Helm's coalescing rules so an override behaves the way a chart
/// author would expect.
fn merge_into(dst: &mut Value, src: &Value) {
    let (Value::Object(d), Value::Object(s)) = (&mut *dst, src) else {
        if !src.is_null() {
            *dst = src.clone();
        }
        return;
    };
    for (k, v) in s {
        match (d.get_mut(k), v) {
            (Some(existing @ Value::Object(_)), Value::Object(_)) => merge_into(existing, v),
            _ => {
                d.insert(k.clone(), v.clone());
            }
        }
    }
}

/// trim_err keeps errors readable. A template failure repeated across four
/// thousand customers is unusable at full length.
fn trim_err(s: &str) -> String {
    let first = s.lines().next().unwrap_or(s);
    if first.len() > 300 {
        format!("{}...", &first[..300])
    } else {
        first.to_string()
    }
}
