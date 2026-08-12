//! In-process benchmark of the Rust renderer, mirroring the Go benchmarks so
//! the two sets of numbers are directly comparable.
//!
//! Deliberately not criterion: the Go side reports best-of-N wall time for
//! whole-request work, and matching that measurement is worth more here than
//! criterion's statistics would be.

use std::path::PathBuf;
use std::sync::Arc;
use std::time::Instant;

use anyhow::Result;
use rust_hedp::blueprint::Blueprint;
use rust_hedp::catalog::Catalog;
use rust_hedp::customer::Config;
use rust_hedp::render::{Options, Renderer};

fn main() -> Result<()> {
    let args: Vec<String> = std::env::args().collect();
    let library = args
        .iter()
        .position(|a| a == "--library")
        .and_then(|i| args.get(i + 1))
        .cloned()
        .unwrap_or_else(|| "../golang-hedp/testdata/library-jinja".into());
    let iterations: usize = args
        .iter()
        .position(|a| a == "--iterations")
        .and_then(|i| args.get(i + 1))
        .and_then(|s| s.parse().ok())
        .unwrap_or(8);

    let root = PathBuf::from(&library);

    // Load, and report it: this is the cost paid once so no request pays it.
    let load_start = Instant::now();
    let catalog = Arc::new(Catalog::load(&root.join("charts"))?);
    let load = load_start.elapsed();
    let blueprint = Arc::new(Blueprint::load(&root.join("blueprint.yaml"))?);

    let s = catalog.stats();
    println!("library: {} charts, {:.1} MB templates, {:.1} MB files, {:.1} MB crds",
        s.charts,
        s.template_bytes as f64 / (1 << 20) as f64,
        s.file_bytes as f64 / (1 << 20) as f64,
        s.crd_bytes as f64 / (1 << 20) as f64);
    println!("load+compile: {} ms\n", load.as_millis());

    let configs: Vec<(&str, Config)> = vec![
        ("minimal", Config { id: "bench-min".into(), tier: "free".into(), region: "eu-west-1".into(), ..Default::default() }),
        ("typical", Config { id: "bench-typ".into(), tier: "standard".into(), region: "eu-west-1".into(),
            features: vec!["observability".into(), "mesh".into()], ..Default::default() }),
        ("heavy", Config { id: "bench-hvy".into(), tier: "premium".into(), region: "us-east-1".into(),
            features: vec!["observability".into(), "mesh".into(), "search".into(), "analytics".into(), "cdn".into()], ..Default::default() }),
        ("all", Config { id: "bench-all".into(), tier: "enterprise".into(), region: "us-east-1".into(),
            render_all: true, ..Default::default() }),
    ];

    for width in [1usize, 4] {
        let renderer = Renderer::new(
            catalog.clone(),
            blueprint.clone(),
            Options { release_concurrency: width, include_manifests: false },
        )?;
        println!("release_concurrency={width}");
        for (name, cfg) in &configs {
            let mut best = u128::MAX;
            let mut last = None;
            for _ in 0..iterations {
                let t = Instant::now();
                let res = renderer.render(cfg);
                let e = t.elapsed().as_micros();
                if !res.ok {
                    anyhow::bail!("{name} failed: {:?}", res.problems);
                }
                best = best.min(e);
                last = Some(res);
            }
            let r = last.unwrap();
            println!("  {:8} {:6.1} ms   releases {:2}  manifests {:3}  {:.1} MB out",
                name, best as f64 / 1000.0, r.planned, r.manifests,
                r.total_bytes as f64 / (1 << 20) as f64);
        }
        println!();
    }

    // Bulk: many customers, each rendered serially, parallelism across
    // customers - the same shape the Go bulk benchmark uses.
    let renderer = Arc::new(Renderer::new(
        catalog.clone(),
        blueprint.clone(),
        Options { release_concurrency: 1, include_manifests: false },
    )?);
    let batch = synthetic_configs(200);
    for conc in [1usize, 2, 4, 8] {
        let pool = rayon::ThreadPoolBuilder::new().num_threads(conc).build()?;
        let t = Instant::now();
        let results: Vec<_> = pool.install(|| {
            use rayon::prelude::*;
            batch.par_iter().map(|c| renderer.render(c)).collect()
        });
        let elapsed = t.elapsed();
        let mut lat: Vec<u64> = results.iter().map(|r| r.micros).collect();
        lat.sort_unstable();
        let bytes: usize = results.iter().map(|r| r.total_bytes).sum();
        println!("bulk concurrency={conc}: {:.1} configs/s, p50 {:.1} ms, p99 {:.1} ms, {:.1} MB/s",
            batch.len() as f64 / elapsed.as_secs_f64(),
            lat[lat.len() / 2] as f64 / 1000.0,
            lat[lat.len() * 99 / 100] as f64 / 1000.0,
            bytes as f64 / (1 << 20) as f64 / elapsed.as_secs_f64());
    }

    Ok(())
}

fn synthetic_configs(n: usize) -> Vec<Config> {
    let tiers = ["free", "standard", "premium", "enterprise"];
    let regions = ["eu-west-1", "us-east-1", "ap-south-1", "eu-central-2"];
    let feature_sets: Vec<Vec<String>> = vec![
        vec![],
        vec!["observability".into()],
        vec!["observability".into(), "mesh".into()],
        vec!["observability".into(), "mesh".into(), "search".into()],
        vec!["analytics".into(), "cdn".into()],
        vec!["observability".into(), "mesh".into(), "search".into(), "analytics".into(), "cdn".into(), "ml".into()],
    ];
    (0..n)
        .map(|i| Config {
            id: format!("cust-{i:05}"),
            tier: tiers[i % tiers.len()].into(),
            region: regions[i % regions.len()].into(),
            features: feature_sets[i % feature_sets.len()].clone(),
            replicas: (1 + i % 5) as i64,
            ..Default::default()
        })
        .collect()
}
