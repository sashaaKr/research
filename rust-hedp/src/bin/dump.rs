//! Renders one customer configuration and writes every manifest as JSON on
//! stdout, keyed the same way the Go dumpmanifests tool keys them.
//!
//! The pair exists to prove the two services render the same bytes. Without
//! that, the benchmark compares two different amounts of work.

use std::collections::BTreeMap;
use std::path::PathBuf;
use std::sync::Arc;

use anyhow::{bail, Result};
use rust_hedp::blueprint::Blueprint;
use rust_hedp::catalog::Catalog;
use rust_hedp::customer::Config;
use rust_hedp::render::{Options, Renderer};

fn main() -> Result<()> {
    let args: Vec<String> = std::env::args().collect();
    let arg = |name: &str, default: &str| -> String {
        args.iter()
            .position(|a| a == name)
            .and_then(|i| args.get(i + 1))
            .cloned()
            .unwrap_or_else(|| default.into())
    };

    let library = arg("--library", "../golang-hedp/testdata/library-jinja");
    let root = PathBuf::from(&library);
    let features: Vec<String> = {
        let f = arg("--features", "");
        if f.is_empty() {
            vec![]
        } else {
            f.split(',').map(|s| s.to_string()).collect()
        }
    };

    let catalog = Arc::new(Catalog::load(&root.join("charts"))?);
    let blueprint = Arc::new(Blueprint::load(&root.join("blueprint.yaml"))?);
    let renderer = Renderer::new(
        catalog,
        blueprint,
        Options { release_concurrency: 1, include_manifests: true },
    )?;

    let cfg = Config {
        id: arg("--id", "acme"),
        tier: arg("--tier", "premium"),
        region: arg("--region", "eu-west-1"),
        features,
        render_all: args.iter().any(|a| a == "--all"),
        ..Default::default()
    };

    let res = renderer.render(&cfg);
    if let Some(p) = &res.problems {
        bail!("validation: {p:?}");
    }

    let mut out: BTreeMap<String, String> = BTreeMap::new();
    for rel in &res.releases {
        if !rel.error.is_empty() {
            bail!("release {}: {}", rel.name, rel.error);
        }
        if let Some(files) = &rel.files {
            for (name, body) in files {
                out.insert(format!("{}/{}", rel.name, normalise(name)), body.clone());
            }
        }
    }

    serde_json::to_writer(std::io::stdout(), &out)?;
    eprintln!(
        "{} releases, {} manifests, {} bytes",
        res.planned, res.manifests, res.total_bytes
    );
    Ok(())
}

fn normalise(name: &str) -> String {
    let n = name.rsplit("/templates/").next().unwrap_or(name);
    n.trim_end_matches(".yaml").trim_end_matches(".j2").to_string()
}
