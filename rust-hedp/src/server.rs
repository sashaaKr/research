//! The HTTP surface, matching the Go service's shape closely enough that the
//! same client can drive both.
//!
//! Scope is deliberately the render path only: `/v1/render` and the streaming
//! NDJSON `/v1/render/bulk`, plus probes. The Go service's multi-version
//! registry, bundle fetcher and admission control are architecture, already
//! measured there, and porting them would not move the number this project
//! exists to produce.

use std::sync::Arc;
use std::time::Instant;

use axum::body::Body;
use axum::extract::State;
use axum::http::{header, StatusCode};
use axum::response::{IntoResponse, Response};
use axum::routing::{get, post};
use axum::{Json, Router};
use serde::{Deserialize, Serialize};
use tokio::sync::mpsc;

use crate::customer::Config;
use crate::render::{RenderResult, Renderer};

#[derive(Clone)]
pub struct AppState {
    pub renderer: Arc<Renderer>,
    /// pool is built once at startup and shared. Building one per request
    /// spawns a fresh set of OS threads on every call, which is pure overhead
    /// and - worse - lets total in-flight threads grow with request
    /// concurrency instead of staying bounded by the core count.
    pub pool: Arc<rayon::ThreadPool>,
    pub max_configs: usize,
}

pub fn router(state: AppState) -> Router {
    Router::new()
        .route("/healthz", get(|| async { Json(serde_json::json!({"status":"ok"})) }))
        .route("/readyz", get(|| async { Json(serde_json::json!({"status":"ok","ready":true})) }))
        .route("/v1/library", get(library))
        .route("/v1/render", post(render_one))
        .route("/v1/render/bulk", post(render_bulk))
        .with_state(state)
}

async fn library(State(st): State<AppState>) -> impl IntoResponse {
    let cat = st.renderer.catalog();
    Json(serde_json::json!({
        "catalog": cat.stats(),
        "charts": cat.names(),
        "releases": st.renderer.blueprint().names(),
        "features": st.renderer.blueprint().features(),
    }))
}

async fn render_one(State(st): State<AppState>, Json(cfg): Json<Config>) -> Response {
    // Rendering is CPU-bound and can run for tens of milliseconds; keeping it
    // off the async runtime's worker threads is the same discipline the Go
    // service gets for free from its scheduler.
    let renderer = st.renderer.clone();
    let res = tokio::task::spawn_blocking(move || renderer.render(&cfg))
        .await
        .expect("render task panicked");

    let status = if res.problems.is_some() {
        StatusCode::UNPROCESSABLE_ENTITY
    } else {
        StatusCode::OK
    };
    (status, Json(res)).into_response()
}

#[derive(Deserialize)]
struct BulkRequest {
    configs: Vec<Config>,
    /// Accepted for wire compatibility with the Go service's client and
    /// deliberately ignored: this process renders through one shared pool
    /// sized to the core count, so letting a request widen it would only
    /// oversubscribe the CPU and inflate everyone's latency.
    #[serde(default, rename = "concurrency")]
    _concurrency: usize,
}

#[derive(Serialize)]
struct Envelope<'a> {
    #[serde(rename = "type")]
    kind: &'a str,
    #[serde(skip_serializing_if = "Option::is_none")]
    result: Option<RenderResult>,
    #[serde(skip_serializing_if = "Option::is_none")]
    summary: Option<BulkSummary>,
}

#[derive(Default, Serialize)]
struct BulkSummary {
    configs: usize,
    ok: usize,
    invalid: usize,
    failed: usize,
    releases: usize,
    manifests: usize,
    total_bytes: u64,
    millis: u64,
    configs_per_second: f64,
    p50_micros: u64,
    p95_micros: u64,
    p99_micros: u64,
}

/// render_bulk streams NDJSON: one line per customer as it completes, then a
/// summary line. Same contract as the Go service, for the same reason - a
/// batch of thousands produces gigabytes of manifests and buffering them to
/// build one document is how the process dies.
async fn render_bulk(State(st): State<AppState>, Json(req): Json<BulkRequest>) -> Response {
    if req.configs.is_empty() {
        return (
            StatusCode::BAD_REQUEST,
            Json(serde_json::json!({"error":"configs must not be empty"})),
        )
            .into_response();
    }
    if req.configs.len() > st.max_configs {
        return (
            StatusCode::PAYLOAD_TOO_LARGE,
            Json(serde_json::json!({
                "error": format!("{} configs exceeds the limit of {}", req.configs.len(), st.max_configs)
            })),
        )
            .into_response();
    }

    let (tx, mut rx) = mpsc::channel::<Vec<u8>>(64);
    let renderer = st.renderer.clone();
    let pool = st.pool.clone();

    tokio::task::spawn_blocking(move || {
        use rayon::prelude::*;
        use std::sync::Mutex;

        let start = Instant::now();
        let total = req.configs.len();

        // Emit each result as it finishes rather than collecting the batch
        // first. Buffering would hold every result in memory and send nothing
        // until the slowest customer completed - the same reason the Go
        // service streams.
        let stats = Mutex::new((BulkSummary { configs: total, ..Default::default() },
                                Vec::<u64>::with_capacity(total)));

        pool.install(|| {
            req.configs.par_iter().for_each(|c| {
                let res = renderer.render(c);

                let line = {
                    let mut guard = stats.lock().expect("summary mutex");
                    let (summary, latencies) = &mut *guard;
                    latencies.push(res.micros);
                    summary.releases += res.releases.len();
                    summary.manifests += res.manifests;
                    summary.total_bytes += res.total_bytes as u64;
                    if res.problems.is_some() {
                        summary.invalid += 1;
                    } else if res.ok {
                        summary.ok += 1;
                    } else {
                        summary.failed += 1;
                    }
                    let env = Envelope { kind: "result", result: Some(res), summary: None };
                    match serde_json::to_vec(&env) {
                        Ok(mut l) => {
                            l.push(b'\n');
                            l
                        }
                        Err(_) => return,
                    }
                };
                // A closed receiver means the client went away. Dropping the
                // line is enough; the remaining renders finish and the task
                // ends on its own.
                let _ = tx.blocking_send(line);
            });
        });

        let (mut summary, mut latencies) = stats.into_inner().expect("summary mutex");

        latencies.sort_unstable();
        let pct = |p: usize| -> u64 {
            if latencies.is_empty() {
                0
            } else {
                latencies[((p * latencies.len()) / 100).min(latencies.len() - 1)]
            }
        };
        let elapsed = start.elapsed();
        summary.millis = elapsed.as_millis() as u64;
        summary.configs_per_second = summary.configs as f64 / elapsed.as_secs_f64().max(1e-9);
        summary.p50_micros = pct(50);
        summary.p95_micros = pct(95);
        summary.p99_micros = pct(99);

        let env = Envelope { kind: "summary", result: None, summary: Some(summary) };
        if let Ok(mut line) = serde_json::to_vec(&env) {
            line.push(b'\n');
            let _ = tx.blocking_send(line);
        }
    });

    let stream = async_stream::stream! {
        while let Some(chunk) = rx.recv().await {
            yield Ok::<_, std::io::Error>(chunk);
        }
    };

    Response::builder()
        .status(StatusCode::OK)
        .header(header::CONTENT_TYPE, "application/x-ndjson")
        .body(Body::from_stream(stream))
        .expect("build streaming response")
}
