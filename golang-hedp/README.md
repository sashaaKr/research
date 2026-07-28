# golang-hedp

A Go HTTP service that renders Helm releases on demand, and the benchmarks
that say how fast that can go.

The question this project exists to answer: **if a platform has ~55 MB of Helm
charts, a dependency graph between releases, and a bulk API that accepts
thousands of customer configurations per request, what does that actually
cost?**

## The short answer

On 4 vCPUs, with a 55 MB library of 40 charts and 40 releases:

| | stock Helm engine | tuned |
|---|---|---|
| One typical customer (24 releases, 225 manifests, 10.5 MB out) | 153 ms | **40 ms** |
| One customer, entire library (40 releases, 382 manifests, 18.5 MB out) | 274 ms | **69 ms** |
| Bulk throughput | 19 configs/s | **~10 configs/s/core** |
| A 1,000-config bulk request | ~53 s | **23-32 s** |
| A 4,000-config bulk request | ~3.5 min | **1.5-2 min** |

"Tuned" is `-cached-engine` plus `GOGC=400`, and for the single-request rows
`-release-concurrency 4`. See [Capacity planning](#capacity-planning-what-to-actually-expect)
for the full picture, including a ±30% hardware variance band that this study
ran into first-hand.

Three findings drove every design decision here:

1. **59% of render time is Helm re-parsing templates that never change.** Helm's
   engine calls `template.Parse` on every file of every chart on every single
   `Render` (`pkg/engine/engine.go`, in `render()`). A CLI pays that once. A
   service pays it forever. Hoisting the parse to startup is worth ~2.2x.
2. **"55 MB of charts" is not a workload description.** Only the `templates/`
   bytes are parsed. In this library that is 19 MB of the 55; the other 36 MB
   is `crds/` (never touched by the engine) and `files/` (read only if a
   template asks). Charts that are big because they vendor CRDs are nearly free
   to render. Charts that are big because they inline dashboards are not.
3. **Useful concurrency stops at the core count, in both directions.** Widening
   past `GOMAXPROCS` does not just fail to help - at 2x oversubscription bulk
   throughput *drops* 19%, and p99 latency gets 3.3x worse.

## What it does

Given a customer configuration, the service:

1. **Validates** it - id format, tier, region, known feature names, override
   depth and size. Rendering 55 MB of charts to discover a misspelled tier is
   the expensive way to find out.
2. **Plans** - selects the releases the customer's features enable, pulls in
   whatever those depend on transitively, and partitions the result into
   dependency waves.
3. **Renders** wave by wave, in parallel within a wave, feeding each release's
   exported values to its dependents.
4. **Reports** a per-release verdict: manifest count, byte count, content
   digest, duration, and any error.

Dependencies are between *releases*, not charts. Helm resolves subchart
dependencies inside one render; these edges span renders, which is what makes
ordering - and therefore parallelism - a real question.

## Layout

```
cmd/chartgen    generates the synthetic 55 MB chart library + blueprint
cmd/hedp        the HTTP service
cmd/loadgen     drives the bulk API and reports client-observed latency

internal/catalog       loads charts into memory, from a directory or a bundle
internal/library       several revisions resident at once, keyed by commit
internal/blueprint     the release graph: gates, edges, exports, topo waves
internal/customer      the input type and its validation rules
internal/render        planning, wave scheduling, bulk fan-out, verdict cache
internal/render/cachedengine   a port of Helm's engine that parses once
internal/server        routes, streaming NDJSON bulk handler, middleware
```

## Running it

The chart library is a build artifact, not source - `testdata/` is gitignored.
Generate it first:

```sh
just gen                     # or: go run ./cmd/chartgen -out testdata/library
just serve-fast              # or: go run ./cmd/hedp -cached-engine
```

```sh
# What was loaded, and how it splits
curl -s localhost:8080/v1/library

# What would render, without rendering it (microseconds)
curl -s -X POST localhost:8080/v1/plan \
  -d '{"id":"acme","tier":"premium","region":"eu-west-1","features":["observability","mesh"]}'

# Render one customer
curl -s -X POST localhost:8080/v1/render \
  -d '{"id":"acme","tier":"premium","region":"eu-west-1","features":["observability","mesh"]}'

# ...with the manifests attached
curl -s -X POST 'localhost:8080/v1/render?manifests=true' -d '{...}'

# Bulk: NDJSON, streamed as each customer finishes, summary line last
curl -s -X POST localhost:8080/v1/render/bulk \
  -d '{"configs":[{...},{...}],"concurrency":4}'
```

Every rendering endpoint takes `?revision=<sha>` to pin the release set to a
commit; without it they use the registry's default. See
[Versioned release sets](#versioned-release-sets-held-in-ram).

## Benchmark results

All numbers: Intel Xeon @ 2.10GHz, 4 vCPU, 16 GB, Go 1.26, Helm SDK v3.21.3.
Reproduce with `just bench-core`.

### The library

```
40 charts, 40 releases, 8 feature gates, 3 dependency waves deep
templates/  19.3 MB   582 files   parsed + executed every render
files/      13.8 MB   128 files   read via .Files, never parsed
crds/       22.0 MB   104 files   never touched by the engine
total       55.0 MB   814 files
```

Cold start - loading and YAML-parsing all of it - is **19 ms**. Startup is not
a problem; this is the cost you pay once so that no request pays it.

### One customer, end to end

Stock Helm engine, releases rendered serially:

| configuration | releases | manifests | rendered | latency | allocations |
|---|---|---|---|---|---|
| minimal (no features) | 14 | 147 | 7.4 MB | 105 ms | 837 k |
| typical (2 features) | 24 | 225 | 10.5 MB | 153 ms | 1.20 M |
| heavy (5 features) | 33 | 302 | 13.9 MB | 209 ms | 1.60 M |
| entire library | 40 | 382 | 18.5 MB | 274 ms | 2.09 M |

Latency tracks rendered output almost linearly (~15 ms per MB of manifests),
not the size of the library on disk.

**Planning the same requests costs 12-57 µs** - four orders of magnitude less
than rendering them. That is why `/v1/plan` exists as its own endpoint: a
caller can see the blast radius of a feature flag for free.

### Where the time goes

Decomposing one render of the largest chart (3.1 MB of templates):

| stage | time | share |
|---|---|---|
| `chartutil.ToRenderValues` (deep-copies values twice) | 0.08 ms | 0.2% |
| `template.Parse` of every file | 24.9 ms | **59%** |
| template execution | 17.5 ms | 41% |
| **`engine.Render` total** | **42.4 ms** | 100% |

The parse depends only on the chart. The chart is immutable. Helm does it
anyway, every time.

### Parsing once

`internal/render/cachedengine` parses each chart into a master template at
startup, then per render clones it - which copies the template namespace and
function maps but *shares the parse trees* - rebinds the two functions that
must close over the clone (`include` and `tpl`), and executes.

| | Helm engine | cached | speedup |
|---|---|---|---|
| Largest chart, one render | 46.5 ms | 19.9 ms | **2.33x** |
| ...allocations | 303 k | 84 k | 3.6x fewer |
| Typical customer, end to end | 157 ms | 73 ms | 2.17x |
| Entire library, end to end | 276 ms | 124 ms | 2.24x |
| Bulk, 200 configs, 4 cores | 18.9 configs/s | 44.1 configs/s | 2.34x |
| Bulk p99 | 358 ms | 160 ms | 2.24x better |

Compiling the largest chart takes 26 ms, once, at boot.

**This is a fork of Helm's engine, and forks rot.** Every Helm minor release is
a diff to re-read against `pkg/engine`. What makes it usable is that
equivalence is tested, not assumed: `TestMatchesHelmEngine` renders all 40
charts through both engines with values that exercise every conditional path
and demands byte-identical output, and `TestCachedEngineMatchesHelm` does the
same through the full service pipeline comparing per-release digests. Keep both
in CI and a Helm upgrade that changes semantics fails loudly. It is off by
default; turn it on with `-cached-engine`.

### Parallelism

Rendering one customer's independent releases in parallel (entire library):

| wave width | latency | speedup |
|---|---|---|
| 1 | 268 ms | 1.0x |
| 2 | 207 ms | 1.29x |
| 4 | 142 ms | 1.89x |
| 8 | 142 ms | 1.89x |

Bulk throughput, 200 configs, each customer rendered serially:

| customers in parallel | throughput | p99 |
|---|---|---|
| 1 | 6.1 configs/s | 270 ms |
| 2 | 11.8 configs/s | 308 ms |
| 4 | 19.0 configs/s | 346 ms |
| 8 | 15.4 configs/s | **1147 ms** |

Both saturate at the core count. Past it, bulk throughput *falls* and p99 blows
up 3.3x, because oversubscribed renders interleave and every customer's wall
time stretches. The service therefore bounds total in-flight renders with one
global semaphore, no matter how the per-request knobs are set.

The two knobs are for different goals, and they compete for the same cores:

- **`-release-concurrency`** cuts single-request latency. Worth turning up when
  requests arrive one at a time.
- **`-bulk-concurrency`** raises throughput. Under bulk load, set
  `-release-concurrency 1`: the cores are already saturated by other customers,
  and serial execution keeps peak memory to one render at a time. This is the
  default.

### Over real HTTP

The benchmarks above measure the renderer. `cmd/loadgen` measures the service -
JSON decode, render, NDJSON encode, and a socket in between. Cached engine,
default settings, 4 cores:

| batch | wall clock | throughput | first result | p50 / p95 / p99 | manifests | rendered |
|---|---|---|---|---|---|---|
| 200 configs | 4.8 s | 41.9 configs/s | 592 ms | 89 / 148 / 171 ms | 45,574 | 2.2 GB |
| 1,000 configs | 23.2 s | 43.2 configs/s | 591 ms | 87 / 150 / 170 ms | 228,385 | 11.1 GB |

Three things worth noting:

- **HTTP costs ~5%** against the in-process benchmark (41.9 vs 44.1 configs/s).
  The service is not the bottleneck; the template engine is.
- **Everything is flat from 200 to 1,000 configs** - throughput, percentiles,
  time to first result. The 1,000-config prediction of ~23 s came out at 23.2 s.
  Batch size is not a variable, which is what you want from a bulk API.
- **11.1 GB of manifests moved through a process holding 363 MB RSS.** That is
  the streaming design doing its job; buffering a JSON array instead would have
  needed the batch to fit in memory.

## Versioned release sets, held in RAM

A release set corresponds to a commit, so a request has to render against the
charts *as they were at that revision*. The service keeps several revisions
resident at once, keyed by revision, and never touches the filesystem to serve
one.

```sh
# Push a revision as a bundle. No checkout involved on the server.
loadgen -revision 9f3c1ab -push ./library

# Render against it explicitly
curl -X POST 'localhost:8080/v1/render?revision=9f3c1ab' -d '{...}'

# What is resident, and what it costs
curl -s localhost:8080/v1/versions

curl -X POST localhost:8080/v1/versions/9f3c1ab/default
curl -X DELETE localhost:8080/v1/versions/old-sha
```

Naming a revision that is not resident is a **404, never a fallback to the
default**. A service whose entire purpose is "render exactly commit X" must not
quietly render commit Y and report success.

### Rendering was already entirely in RAM

Worth stating plainly, because it is the first thing to check: charts are read
from disk **once at startup** and rendering never goes near the filesystem
again. A `*chart.Chart` holds every template and file as `[]byte` in the heap,
and `engine.Render` only ever walks those. There is no per-request I/O to
remove, so there is no rendering speedup available from "keep it in RAM" - that
was already banked in the first version of this service.

What the in-memory path actually buys is the versioning above: a bundle can be
fetched from git, an OCI registry or an object store and loaded straight out of
the byte slice it arrived in (`loader.LoadFiles` / `loader.LoadArchive`), so N
revisions cost N heap allocations instead of N checkouts.

### Loading a version: the numbers

| source | time | notes |
|---|---|---|
| Directory (`catalog.Load`) | **17 ms** | 814 files from page cache, read in parallel |
| Bundle, one gzip for the whole library | 192 ms | decompression pinned to one core |
| Bundle, one gzip per chart | **77 ms** | decompression spread across cores |

The bundle path is *slower* than reading a checkout, not faster - which is the
opposite of what "keep it in RAM" suggests. Page-cache reads parallelise across
goroutines; a single gzip stream cannot be parallelised at all. Isolating it
confirmed the cause: **87% of the naive bundle's load time is gunzip** (169 ms
of 192 ms).

Hence the packed layout - an uncompressed outer tar holding one `.tgz` per
chart, which is also Helm's native packaging. One stream per chart means
decompression scales with cores, and compressing already-compressed charts was
never buying anything. That is 2.5x, and `catalog.LoadBundle` accepts either
layout.

None of this is in the request path. It is what a "load this commit" call
costs, once per revision.

### Memory per resident version

| | measured heap | registry estimate |
|---|---|---|
| Charts only (55 MB raw) | 58.6 MB | 60.5 MB |
| Charts + compiled templates | 133.5 MB | 137.5 MB |

So a version costs roughly **1.07x its raw size**, or **2.4x with
`-cached-engine`** - the parsed template trees are not free. The estimator the
registry uses for admission control is within 3% of measured, which is what
makes the memory budget trustworthy rather than decorative.

At ~134 MB a version, a 4 GB budget holds about 30 revisions; charts-only, ~68.
Set it with `-memory-budget-mb`.

Two caveats on that number:

- **It accounts for resident library data, not process RSS.** Rendering churns
  a lot of short-lived memory: with two versions resident (275 MB of libraries)
  and 200 configs in flight, RSS peaked at 925 MB. Size the container for the
  render working set, not just the budget.
- **A bundle is 13.4 MB on the wire** against 55 MB raw, so shipping revisions
  around is cheap even though holding them is not.

### Eviction

Least-recently-used, bounded by the memory budget, with two deliberate rules:

- **The default revision is never evicted.** Dropping it would silently
  redirect every unqualified request to a different commit.
- **A load that cannot fit is refused with an error**, not admitted over
  budget. An early version of this quietly exceeded the limit whenever the
  pinned default left too little room - a budget you silently exceed is not a
  budget, and the operator finds out from the OOM killer instead of from an
  error message. It now says exactly what is pinned and by how much it is
  short.

Concurrent loads of the same cold revision collapse into one: at ~134 MB
apiece, a burst of requests for a revision that is not resident must not each
load their own copy.

## Design notes

**Charts are loaded once per revision and shared by every goroutine.** Helm deep-copies
chart values before touching them (`chartutil.coalesceValues`) and template
execution does not mutate parse trees, so a `*chart.Chart` is safe to render
concurrently. `TestConcurrentRendersAreSafe` pins this down under `-race`;
without it the whole design collapses back to per-request loading.

**Bulk responses stream.** Thousands of configs against this library produce
gigabytes of manifests. The bulk endpoint emits NDJSON as each customer
finishes and a summary line at the end, so memory stays proportional to
concurrency rather than to batch size. It also means a client sees the first
verdict in milliseconds rather than after the slowest customer.

**Bulk renders return verdicts, not payloads.** The question is "does this
render", so the default response carries manifest counts, byte counts and a
content digest. The digest is the useful part: it is how you tell that a
customer's output changed without diffing 10 MB of YAML. Manifests are
opt-in per request (`?manifests=true`).

**A failed release skips its dependents** rather than rendering them against
values they were never written for. The result reports the blocking release, so
one broken chart produces one error and a list of consequences instead of
twenty confusing ones.

**Validation failures are 422 with field-level problems; template failures are
200 with per-release errors.** In a bulk world the caller still needs the
verdicts for the other 3,999 configs.

## What the verdict cache is and isn't worth

`-cache N` memoises `(chart, values) -> verdict`. Within a batch it only helps
where configs are genuinely identical, and every rendered manifest carries the
customer id in its labels, so *distinct customers never collide*. The cache
pays for itself when a batch contains repeats - retries, re-submissions, the
same tenant listed under several accounts - and costs a hash of the values tree
when it does not.

Measured on 200-config batches with a cold cache each time:

| duplicate share | no cache | with cache | hit rate | speedup |
|---|---|---|---|---|
| 0% | 18.1 configs/s | 19.0 configs/s | 0% | none |
| 50% | 19.3 configs/s | 37.1 configs/s | 49.8% | 1.9x |
| 90% | 18.3 configs/s | 163.6 configs/s | 89.4% | 8.9x |

Throughput scales as roughly `1 / (1 - hit rate)`, which is what you would
expect if the cache is doing exactly one thing and the hashing is free. At 0%
duplicates the cost of hashing the values tree is inside the noise, so leaving
the cache on is close to free insurance - but it earns nothing on a batch of
genuinely distinct customers, and no amount of tuning changes that.

Do not read a warm-cache benchmark as a throughput number. Replaying the same
batch into an already-full cache measures the cache, not the service; the
figures above reset the cache before every iteration for that reason.

## Capacity planning: what to actually expect

### A note on variance first

This container was rescheduled onto different hardware mid-study (Xeon 2.10GHz
then 2.80GHz, 4 vCPU both times), and the second machine ran **25-30% slower**
on identical code despite the higher nominal clock - shared, contended
hardware. Every ratio below reproduced across both machines. Every absolute
number should be read with a ±30% band until measured on your own hardware.
The per-core figures are the portable ones.

### Expected latency, single request

Latency-tuned (`-cached-engine -release-concurrency 4`), measured over HTTP on
the slower machine, so treat these as the pessimistic end:

| request | releases | rendered | latency |
|---|---|---|---|
| typical customer | 24 | 10.5 MB | **40 ms** |
| heavy customer | 33 | 13.9 MB | **66 ms** |
| entire library | 40 | 18.5 MB | **69 ms** |

**Any single customer renders in under 100 ms**, and the spread between a small
customer and the whole library is under 2x. Validation-only rejection (bad
tier, unknown feature) returns in well under a millisecond, because nothing
renders.

Throughput-tuned (`-release-concurrency 1`), the same requests cost 112 / 150 /
156 ms median. That is the trade: serial releases free the cores for other
customers.

### Expected latency, bulk request

Bulk is throughput-bound, so wall time is just `configs / throughput`:

| batch | 4 cores | 16 cores (projected) |
|---|---|---|
| 100 | ~3 s | ~1 s |
| 1,000 | **23-32 s** | ~7-9 s |
| 4,000 | **91-128 s** | ~26-36 s |
| 10,000 (the API cap) | ~4-5 min | ~65-90 s |

Per-customer latency *within* a batch is p50 ~90-100 ms, p95 ~150-310 ms,
p99 ~170-450 ms depending on machine contention. Those were flat from 200 to
1,000 configs - batch size does not degrade them - and time-to-first-result
stays around 600 ms regardless, because results stream.

### The upper bound, and what sets it

**~8-11 customer configs per second per core.** That is the number to plan
with. On 4 cores: 31-44 configs/s measured end to end.

Three things bound it, in order:

1. **Allocation rate.** A typical customer allocates **67 MB** and the full
   library **116 MB**, even with the cached engine. At 40 configs/s that is
   ~2.7 GB/s of garbage. This, not template execution, is what will stop the
   service scaling linearly with cores.
2. **Parallel efficiency.** Measured scaling was 1.93x at 2 cores and 3.11x at
   4 - about **78% efficiency** - and past `GOMAXPROCS` throughput *falls*.
   The 16-core projections above assume that efficiency holds; they are the
   least trustworthy numbers on this page, because allocation bandwidth gets
   worse with core count, not better.
3. **Output size.** ~15 ms per MB of rendered manifests. A customer's latency
   tracks what they render, not the size of the library.

### Set GOGC

Because the workload is allocation-heavy, the Go default (`GOGC=100`) spends a
quarter of the machine collecting:

| GOGC | bulk throughput | p99 |
|---|---|---|
| 100 (default) | 31.2 configs/s | 219 ms |
| 400 | 38.7 configs/s | 188 ms |
| 800 | **39.8 configs/s** | **170 ms** |

**`GOGC=400` buys 24% for free**, 800 a little more with diminishing returns.
The cost is heap headroom, which this service has: it is holding immutable
chart data, not growing. Pair it with `GOMEMLIMIT` set to the container limit
so the collector still has a hard backstop.

### Sizing a box

For a target of *R* configs/second:

- **Cores**: `R / 10`, plus headroom - the service saturates at `GOMAXPROCS`.
- **Memory**: `(resident versions x 134 MB) + (concurrent renders x ~150 MB
  working set) + GOGC headroom`. With 3 revisions resident and 4-way
  concurrency, 4 GB is comfortable and 2 GB is tight. Observed RSS was 925 MB
  with 2 revisions and 200 configs in flight.
- **Horizontally**: the service is stateless per request once versions are
  loaded, so N replicas give N times the throughput. Push each revision to
  every replica, or let them pull on first miss - the load dedup already
  handles a burst of concurrent misses.

## Extrapolating

Per core, cached engine, mixed feature sets: **~11 configs/s/core**. Stock Helm
engine: ~4.7.

For a bulk request of *N* configs on *C* cores, wall time is roughly
`N / (11 x C)` seconds. That puts a 4,000-config request at ~91 s on 4 cores,
~23 s on 16. Whether that is acceptable is a product question, but the shape is
clear: this workload is embarrassingly parallel and CPU-bound, so it scales
with cores until the box runs out, and horizontally after that. Nothing here
touches I/O or the network after startup.

Three things would move the number further, in order of expected value:

1. **Render only what changed.** Most customers' configs do not change between
   requests. Keying a persistent cache on the customer's config hash and
   returning stored digests turns re-validation into a lookup. This is where
   the remaining order of magnitude is, and it is architecture, not
   micro-optimisation.
2. **Stop using SHA-256 for the digest.** With the parse cost gone, a CPU
   profile of the cached engine puts 8.2% of samples in `sha256` - our own
   content digest, not Helm's. The digest exists to detect change, not to
   resist attack, so `maphash` or xxhash would reclaim most of that. Cheap fix,
   measurable win, no fork involved.
3. **Cut the output.** 15 ms per MB of manifests is mostly `strings.Builder`
   growth and map churn in template execution (`text/template.walk` is 48% of
   the cached-engine profile). A service that only needs a verdict could hash
   rendered output through a streaming writer instead of materialising it -
   worth a share of the 41% that execution costs, but a deeper change to the
   engine fork than the parse cache was.

## Tests

```sh
just test      # everything
just verify    # differential vs Helm's engine, plus -race on the shared paths
```

`internal/render/cachedengine/engine_test.go` is the one to keep an eye on: it
is what makes the fork trustworthy.
