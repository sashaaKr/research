//! The Rust HEDP renderer, served over HTTP.

use std::net::SocketAddr;
use std::path::PathBuf;
use std::sync::Arc;

use anyhow::Result;
use clap::Parser;
use rust_hedp::blueprint::Blueprint;
use rust_hedp::catalog::Catalog;
use rust_hedp::render::{Options, Renderer};
use rust_hedp::server::{router, AppState};

#[derive(Parser)]
struct Args {
    #[arg(long, default_value = "127.0.0.1:8090")]
    addr: String,
    #[arg(long, default_value = "../golang-hedp/testdata/library-jinja")]
    library: String,
    /// Parallel releases within one customer. 1 is bulk-tuned.
    #[arg(long, default_value_t = 1)]
    release_concurrency: usize,
    #[arg(long, default_value_t = 0)]
    bulk_concurrency: usize,
    #[arg(long, default_value_t = 10000)]
    max_configs: usize,
}

#[tokio::main]
async fn main() -> Result<()> {
    let args = Args::parse();
    let root = PathBuf::from(&args.library);

    let start = std::time::Instant::now();
    let catalog = Arc::new(Catalog::load(&root.join("charts"))?);
    let blueprint = Arc::new(Blueprint::load(&root.join("blueprint.yaml"))?);
    let s = catalog.stats();
    eprintln!(
        "library loaded: {} charts, {} releases, {} MB templates, {} MB files, {} MB crds, {} ms",
        s.charts,
        blueprint.releases.len(),
        s.template_bytes >> 20,
        s.file_bytes >> 20,
        s.crd_bytes >> 20,
        start.elapsed().as_millis()
    );

    let renderer = Arc::new(Renderer::new(
        catalog,
        blueprint,
        Options {
            release_concurrency: args.release_concurrency,
            include_manifests: false,
        },
    )?);

    let bulk_concurrency = if args.bulk_concurrency > 0 {
        args.bulk_concurrency
    } else {
        std::thread::available_parallelism().map(|n| n.get()).unwrap_or(4)
    };
    // One pool for the process, so total in-flight renders stay bounded by the
    // core count no matter how many requests arrive at once - the same job the
    // Go service's global semaphore does.
    let pool = Arc::new(
        rayon::ThreadPoolBuilder::new()
            .num_threads(bulk_concurrency)
            .build()?,
    );

    let app = router(AppState { renderer, pool, max_configs: args.max_configs });
    let addr: SocketAddr = args.addr.parse()?;
    let listener = tokio::net::TcpListener::bind(addr).await?;
    println!("rust-hedp listening on {addr}");

    axum::serve(listener, app)
        .with_graceful_shutdown(async {
            let _ = tokio::signal::ctrl_c().await;
        })
        .await?;
    Ok(())
}
