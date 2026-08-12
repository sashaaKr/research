# rust-hedp

A Rust port of the [golang-hedp](../golang-hedp) dynamic release renderer,
built to answer one question: **would this service be materially faster in
Rust?**

Short answer: **yes, about 2.2x faster and 15x smaller in memory - and you
almost certainly should not do it.** The reason is the first section.

## Read this before the numbers

**This service cannot render a Helm chart.**

Helm charts are Go `text/template` plus [sprig](https://github.com/Masterminds/sprig).
No Rust library implements that, and "implement it" means reproducing Go's
template semantics, sprig's ~200 functions, and Helm's engine on top. The Go
service already forks ~400 lines of Helm's engine to cache parsing, and keeping
*that* honest needs a differential test against every Helm release. A full
reimplementation in another language is a different order of undertaking.

So this port renders a **Jinja dialect of the same chart library**, generated
from the same source by `chartgen -dialect jinja`. That makes the comparison
fair - it does not make the Rust service a drop-in replacement.

What the comparison measures: the same values, the same release graph, the same
number of templates, the same number of template actions, the same output. What
it does not measure: whether Rust could render your actual charts. It could not.

### The equivalence is verified, not assumed

```sh
./verify-equivalence.sh
```

Renders four config shapes through both services and diffs every manifest:

```
minimal   keys go=147 rust=147 shared=147 differing=0
typical   keys go=225 rust=225 shared=225 differing=0
heavy     keys go=302 rust=302 shared=302 differing=0
all       keys go=382 rust=382 shared=382 differing=0
OK: byte-identical across every config shape
```

Getting there took four rounds of fixes, and each one would have silently
skewed the benchmark: the Jinja templates did not replicate Go's `{{-`
whitespace trimming; the two generators consumed the PRNG in different orders
so the *static* text diverged; the dashboard byte-budget produced different
panel counts per dialect; and MiniJinja strips a template's trailing newline
where Go does not. Before those fixes the Rust service looked faster partly
because it was rendering less. **A language comparison without an output diff
is a coin toss.**

## Results

Same machine, same session, back to back. Intel Xeon @ 2.80GHz, 4 vCPU.
Go is `-cached-engine` with `GOGC=400` - its best configuration, not its
default.

### One customer, in-process, releases rendered serially

| configuration | releases | rendered | Go | Rust | Rust advantage |
|---|---|---|---|---|---|
| minimal | 14 | 7.4 MB | 56.2 ms | **19.9 ms** | 2.8x |
| typical | 24 | 10.5 MB | 68.9 ms | **28.8 ms** | 2.4x |
| heavy | 33 | 13.9 MB | 90.9 ms | **38.7 ms** | 2.4x |
| entire library | 40 | 18.5 MB | 121.7 ms | **51.3 ms** | 2.4x |

With 4-way release parallelism, the full library is 79.6 ms in Go against
**26.7 ms** in Rust - 3.0x.

### Bulk, over HTTP, 400 configs in chunks of 100

| | Go | Rust | Rust advantage |
|---|---|---|---|
| throughput | 52.7 configs/s | **118.3 configs/s** | 2.24x |
| wall clock | 7.59 s | **3.38 s** | 2.24x |
| per-config p50 | 68.1 ms | **31.0 ms** | 2.20x |
| per-config p99 | 143.7 ms | **56.4 ms** | 2.55x |
| **RSS under load** | 1087 MB | **72 MB** | **15x less** |

Both rendered 91,285 manifests and 4.4 GB of output, so the work was identical.

### Startup and idle

| | Go | Rust |
|---|---|---|
| load | 108 ms | 124 ms (load + compile) |
| compile templates | 78 ms | (included above) |
| idle RSS | 187 MB | **69 MB** |

Startup is a wash. Idle memory is not.

## What the numbers actually mean

**The memory difference is the real result.** 1087 MB against 72 MB under
identical load is not a tuning artifact - it is a garbage-collected runtime
holding a 67 MB-per-config allocation rate against an arena-free one that
frees as it goes. It is the difference between sizing pods at 4 Gi and sizing
them at 256 Mi, and it does not go away with more `GOGC` tuning.

**The speed difference is real but not purely a language result.** This is
`text/template` versus MiniJinja as much as it is Go versus Rust, and this
study cannot separate the two. MiniJinja is a newer engine with a value model
built for exactly this; Go's `text/template` is reflection-driven and predates
generics. A fair share of the 2.4x is engine design, not codegen. Treating it
as "Rust is 2.4x faster than Go" would over-claim.

**Both saturate at the core count**, and both lose throughput past it - the
Rust service peaks at 120.5 configs/s at 4-way concurrency and drops to 112.2
at 8-way, the same shape the Go service shows. Nothing about Rust changes the
scaling story from [DEPLOYMENT.md](../golang-hedp/DEPLOYMENT.md).

**One measured mistake worth repeating.** The first Rust HTTP numbers showed
only 1.25x, because the handler collected every result before streaming and
built a fresh rayon thread pool per request. Fixing both took it from 65.9 to
118.3 configs/s and time-to-first-result from 1.68 s to 34 ms. The port was
slower than the original at the same task purely because of how it was written.
A language does not make a service fast.

(Go's 593 ms time-to-first-result is a flush-policy choice - it flushes every
64 results - not a runtime property. Do not read that row as a language
difference.)

## The recommendation

**Stay on Go**, unless something changes.

- The Rust service cannot render Helm charts, and getting it there means
  reimplementing Helm's engine in Rust and keeping it correct across Helm
  releases. That is a permanent tax paid to save half your replicas.
- 2.2x throughput halves your pod count. At the measured ~10 configs/s/core, a
  4000-config batch is 91 s on 4 Go cores against 41 s on 4 Rust cores. Both
  are "a job, not a request"; the architecture in DEPLOYMENT.md handles both
  the same way.
- The memory result is the one worth acting on, and the cheapest way to act on
  it is **in Go**: the allocation rate is already flagged as the next
  optimisation there (67 MB per config, ~2.7 GB/s of garbage at load). Cutting
  it moves both memory and throughput without changing language.

Reconsider if: you abandon Helm chart compatibility for a format you control
anyway; memory per pod becomes the binding cost; or the workload grows an order
of magnitude, at which point 2.2x is the difference between 20 pods and 45.

## Running it

The Jinja library is generated by the Go project's chartgen:

```sh
cd ../golang-hedp && go run ./cmd/chartgen -out testdata/library-jinja -dialect jinja
```

```sh
cargo build --release

./verify-equivalence.sh                       # prove the outputs match first
./target/release/bench --iterations 8         # in-process benchmark
./target/release/hedp --addr 127.0.0.1:8090   # HTTP service

# Drive it with the Go project's load generator - same client, both services
../golang-hedp/bin/loadgen -addr http://127.0.0.1:8090 -configs 400 -chunk 100
```

## Scope

Ported: chart loading, the release graph with dependency waves, customer
validation, the render pipeline with value merging and exports, bulk fan-out,
and the streaming NDJSON HTTP endpoint.

Not ported: the multi-version registry, the bundle fetcher, and admission
control. Those are architecture rather than throughput, they are already
measured in the Go service, and porting them would not move the number this
project exists to produce.
