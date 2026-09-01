//! Tests for the Rust renderer. They mirror the Go service's tests so that a
//! behavioural difference between the two shows up here rather than in a
//! benchmark number nobody can explain.

use std::path::PathBuf;
use std::sync::Arc;

use rust_hedp::blueprint::Blueprint;
use rust_hedp::catalog::Catalog;
use rust_hedp::customer::Config;
use rust_hedp::render::{Options, Renderer};

fn library() -> Option<PathBuf> {
    let p = PathBuf::from("../golang-hedp/testdata/library-jinja");
    p.join("blueprint.yaml").exists().then_some(p)
}

fn renderer(opts: Options) -> Option<Renderer> {
    let root = library()?;
    let catalog = Arc::new(Catalog::load(&root.join("charts")).unwrap());
    let blueprint = Arc::new(Blueprint::load(&root.join("blueprint.yaml")).unwrap());
    Some(Renderer::new(catalog, blueprint, opts).unwrap())
}

#[test]
fn renders_manifests() {
    let Some(r) = renderer(Options { release_concurrency: 1, include_manifests: true }) else {
        eprintln!("skipping: jinja library not generated");
        return;
    };
    let res = r.render(&Config {
        id: "acme-corp".into(),
        tier: "premium".into(),
        region: "eu-west-1".into(),
        features: vec!["observability".into(), "mesh".into()],
        replicas: 3,
        ..Default::default()
    });

    assert!(res.ok, "render failed: {:?}", res.problems);
    assert!(res.planned > 0 && res.manifests > 0);

    // Values must actually reach the templates; a render that dropped the
    // customer identity would still report success.
    let saw = res.releases.iter().any(|rel| {
        rel.files.as_ref().is_some_and(|f| {
            f.values()
                .any(|b| b.contains("hedp.example.com/customer: \"acme-corp\""))
        })
    });
    assert!(saw, "customer id did not reach the manifests");
}

#[test]
fn dependency_exports_flow_downstream() {
    let Some(r) = renderer(Options { release_concurrency: 1, include_manifests: true }) else {
        return;
    };
    let res = r.render(&Config {
        id: "dep-test".into(),
        tier: "enterprise".into(),
        region: "us-east-1".into(),
        render_all: true,
        ..Default::default()
    });
    assert!(res.ok);
    assert!(res.waves >= 2, "expected a multi-wave plan, got {}", res.waves);

    let saw = res.releases.iter().any(|rel| {
        rel.files.as_ref().is_some_and(|f| {
            f.values()
                .any(|b| b.contains("_ENDPOINT") && b.contains("svc.cluster.local:8080"))
        })
    });
    assert!(saw, "no downstream release received an upstream export");
}

#[test]
fn validation_rejects_bad_config() {
    let Some(r) = renderer(Options::default()) else { return };
    let res = r.render(&Config {
        id: "Bad_ID".into(),
        tier: "platinum".into(),
        region: "nowhere".into(),
        features: vec!["observability".into(), "teleportation".into()],
        ..Default::default()
    });

    assert!(!res.ok);
    let problems = res.problems.expect("expected validation problems");
    for field in ["id", "tier", "region", "features"] {
        assert!(problems.contains_key(field), "no problem for {field}: {problems:?}");
    }
    assert!(res.releases.is_empty(), "nothing should render when validation fails");
}

#[test]
fn identical_input_renders_identically_across_threads() {
    let Some(r) = renderer(Options { release_concurrency: 4, include_manifests: false }) else {
        return;
    };
    let r = Arc::new(r);
    let cfg = Config {
        id: "shared-input".into(),
        tier: "standard".into(),
        region: "eu-west-1".into(),
        features: vec!["observability".into()],
        ..Default::default()
    };

    let digests: Vec<String> = std::thread::scope(|s| {
        let handles: Vec<_> = (0..8)
            .map(|_| {
                let (r, cfg) = (r.clone(), cfg.clone());
                s.spawn(move || {
                    let res = r.render(&cfg);
                    assert!(res.ok);
                    res.releases
                        .iter()
                        .map(|rel| format!("{}{}", rel.name, rel.digest))
                        .collect::<String>()
                })
            })
            .collect();
        handles.into_iter().map(|h| h.join().unwrap()).collect()
    });

    for (i, d) in digests.iter().enumerate().skip(1) {
        assert_eq!(d, &digests[0], "thread {i} diverged from thread 0");
    }
}

#[test]
fn plan_pulls_in_dependencies_past_their_gates() {
    let Some(r) = renderer(Options::default()) else { return };
    let plan = r.blueprint().plan_for(&["observability".to_string()], false);
    assert!(plan.count() > 0);
    // A feature-gated release depending on a base release must drag that base
    // release in even though no feature selected it.
    assert!(!plan.pulled_in.is_empty(), "expected a dependency-pulled release");
    assert!(plan.waves.len() >= 2, "expected a multi-wave plan");
}
