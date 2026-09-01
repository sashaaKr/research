//! A Rust port of the HEDP dynamic Helm-release renderer, built to be
//! benchmarked against the Go original.
//!
//! It is a port of the *architecture*, not of Helm. Helm charts are Go
//! text/template plus sprig and no Rust library renders them, so this service
//! renders a Jinja dialect of the same chart library - generated from the same
//! source by `chartgen -dialect jinja`, and asserted to produce byte-identical
//! output. What that buys is a comparison of equal work; what it costs is that
//! this service could not render a real Helm chart.

pub mod blueprint;
pub mod catalog;
pub mod customer;
pub mod filters;
pub mod render;
pub mod server;
