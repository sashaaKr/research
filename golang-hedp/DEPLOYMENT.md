# Running HEDP in Kubernetes

How to shape batches, responses and replicas, given what the benchmarks
actually measured. Numbers are from a 4-core pod against the 55 MB / 40-chart
library; see [README](README.md#capacity-planning-what-to-actually-expect).

## The two measurements that decide the architecture

**1. Chunking a batch is free above ~100 configs.** 1000 configs, one client,
split into sequential requests:

| chunk size | throughput | vs one big request | per-request wall time |
|---|---|---|---|
| 10 | 32.9 configs/s | -16% | 0.31 s |
| 25 | 36.5 configs/s | -7% | 0.68 s |
| 50 | 36.8 configs/s | -6% | 1.36 s |
| 100 | 38.8 configs/s | **-1%** | 2.58 s |
| 250 | 39.1 configs/s | **-0.3%** | 6.42 s |
| 1000 (one request) | 39.2 configs/s | baseline | 25.5 s |

You can turn one 25-second request into ten 2.6-second requests and pay 1% for
it. Everything below follows from that.

**2. One pod is saturated by one client.** 1000 configs in chunks of 100,
varying how many chunks are in flight against a *single* pod:

| concurrent clients | throughput | per-config p50 | per-config p99 |
|---|---|---|---|
| 1 | 37.8 configs/s | 97 ms | 187 ms |
| 2 | 39.1 configs/s | 196 ms | 338 ms |
| 4 | 39.0 configs/s | 364 ms | 653 ms |
| 8 | 38.9 configs/s | 747 ms | 1283 ms |

Throughput is **flat**; latency is **linear** in concurrency. The pod is fully
busy with one caller, because the bulk handler already fans out across every
core internally. Piling on more concurrent requests buys nothing and multiplies
both latency and peak memory.

The consequence is the single most important thing on this page:
**concurrency has to go across pods, not into one pod.** Two replicas double
throughput; two concurrent requests to one replica do not.

## Recommended shape

Stateless replicas behind a normal Service, clients chunk, no coordinator, no
queue.

```
client ──chunks of 100-250, C in flight──▶ Service ──▶ [pod] [pod] [pod] ...
                                                         │
                                      each pulls revisions from artifact store
```

**Why no scatter-gather coordinator.** A pod cannot see its siblings without a
headless Service and client-side load balancing, and a coordinator adds a
failure mode without adding throughput - the client already has all the
parallelism needed. Fan-in bandwidth is not a reason either: a verdict is a few
hundred bytes, so 4000 of them is under a megabyte. (This changes entirely if
you return manifests: 1000 configs is 11 GB. Don't fan those in - see
[Responses](#responses).)

**Why no queue, yet.** At ~39 configs/s per 4-core pod, ten replicas is ~390
configs/s, and a 4000-config batch finishes in about ten seconds. A job system
is the right answer when you need durable results, resumability, or batches
that outlive a client - not for throughput. Build it when a requirement asks
for it, not preemptively.

### The client side

```
1. Pin the revision explicitly. Do not rely on the server default.
2. Split configs into chunks of 100-250.
3. Send C chunks concurrently, where C ≈ replica count. Not more.
4. On 503, honour Retry-After and retry the chunk.
5. On a connection error, retry the chunk.
6. Merge the verdict streams.
```

Retries are safe without any bookkeeping: a render is a pure function of
`(revision, config)`, so re-sending a chunk cannot corrupt anything. That is
the property that makes this whole design cheap - and it is why the response
carries a content digest, so a client can tell a retry produced the same answer.

`cmd/loadgen` implements exactly this, and is the reference:

```sh
loadgen -revision 9f3c1ab -configs 4000 -chunk 200 -clients 10
```

### Why chunking is not optional

Even ignoring throughput, a single 25-second request is fragile in Kubernetes:

- **Ingress timeouts.** nginx-ingress defaults `proxy-read-timeout` to 60s.
  A 4000-config request takes ~100s on one pod and simply dies. Chunks of 200
  finish in ~6s and fit inside every default.
- **Rolling updates and evictions.** A long request is a long window in which
  a pod can be drained. A 6-second chunk is retried and forgotten; a
  100-second batch loses all its work.
- **Failure granularity.** One bad chunk is 200 configs to retry, not 4000.

## Load shedding

Because extra concurrency against a saturated pod is pure loss, the service
refuses it. `-max-in-flight` (default 2) caps concurrent renders and answers
`503` with `Retry-After` beyond that.

This is deliberate: queueing inside the pod *hides* the need for another
replica, while refusing makes it visible to the client, to the load balancer,
and to whatever you scale on. Probes and `/v1/versions` stay answerable at
capacity, so a busy pod is not dropped from rotation for being busy.

Set it to roughly `1-2`. Higher only helps if your chunks are small enough that
per-request overhead dominates, which the table above says they should not be.

**Load balance by least-request, not round-robin.** Round-robin will hand a
chunk to a pod that is already mid-render while another sits idle. With Istio
or Linkerd, use `LEAST_REQUEST`. With plain kube-proxy you get random, which is
adequate once `-max-in-flight` is shedding - the client's retry lands somewhere
else.

## Versions: pods pull, they are never pushed

`PUT /v1/versions/{revision}` behind a Service lands on **one** replica. The
other N-1 never hear about it and will 404 that revision. This is the easiest
mistake to make with this API and it fails in the most confusing way - a
revision that works intermittently, depending on which pod you hit.

So configure a bundle source and let every pod resolve revisions itself:

```
-bundle-url https://artifacts.internal/hedp/{revision}.tar
-revision   9f3c1ab      # pulled at startup, becomes this pod's default
```

- On a cache miss, the pod fetches, loads, and serves - one extra ~400 ms on
  the first request for that revision.
- Concurrent misses for the same revision collapse into **one** fetch. At
  134 MB a copy, a burst for a freshly built commit must not download it once
  per in-flight request.
- Anything that speaks HTTP works: S3/GCS, an artifact service, an OCI registry
  behind a proxy, a sidecar running `git archive`.
- `404`/`403` from the store means the revision does not exist, and the API
  answers 404. Any other failure is an outage and answers 503. A caller can
  tell "you asked for a commit that was never built" from "our artifact store
  is down", and only the second is worth retrying.

Give every replica the same `-revision` so they agree on the default. Better
still, have clients always pin `?revision=` and treat the default as a
convenience for development.

`PUT /v1/versions/{revision}` remains useful for a single-pod dev setup, for
preloading a replica before it takes traffic, and for testing a bundle that is
not in the artifact store yet.

## Probes

```yaml
livenessProbe:                    # is the process alive
  httpGet: { path: /healthz, port: 8080 }
readinessProbe:                   # can it actually render
  httpGet: { path: /readyz, port: 8080 }
  initialDelaySeconds: 2
  periodSeconds: 5
```

They are deliberately different. `/readyz` fails while a pod has no resident
version, so a rolling update does not route traffic to pods still pulling their
bundle. `/healthz` ignores library state entirely - gating liveness on it would
restart a pod forever if the artifact store were briefly down, turning a
degraded dependency into a crash loop.

## Autoscaling

Scale on CPU. The workload is CPU-bound and the correlation is direct.

```yaml
metrics:
  - type: Resource
    resource: { name: cpu, target: { type: Utilization, averageUtilization: 70 } }
```

Two caveats:

- **HPA cannot react to a burst.** Pod start plus version pull is 5-15 seconds,
  and the HPA's own window is longer. A 4000-config batch arriving at once is
  over before new pods are ready. If bursts are your normal traffic, provision
  for the peak or scale on queue depth with KEDA - CPU-based HPA smooths
  sustained load, not spikes.
- **Rate of 503s is the better signal** if you can scrape it. It means "clients
  wanted more throughput than we had", which is exactly the scale-up condition,
  and it responds instantly instead of after a CPU averaging window.

## Resources

```yaml
resources:
  requests: { cpu: "4", memory: "3Gi" }
  limits:   { memory: "4Gi" }        # no CPU limit - see below
env:
  - { name: GOGC,       value: "400" }
  - { name: GOMEMLIMIT, value: "3500MiB" }
  - { name: GOMAXPROCS, value: "4" }
```

- **Memory**: `(resident versions x 134 MB) + (in-flight renders x ~150 MB) +
  GC headroom`. Observed RSS was 925 MB with 2 versions and 200 configs in
  flight. 3-4 Gi is comfortable for 3 resident revisions.
- **`GOGC=400`** is worth 24% throughput on this allocation-heavy workload
  (67 MB per typical config). Pair it with `GOMEMLIMIT` below the container
  limit so the collector still has a hard backstop.
- **`GOMAXPROCS`** must match the CPU request. Go reads the machine's core
  count, not the cgroup quota, so on a large node a small pod will start far
  too many worker threads and thrash. Go 1.25+ reads the cgroup limit
  automatically, but setting it explicitly costs nothing and is unambiguous.
- **No CPU limit.** Throttling a workload that is deliberately using every core
  for short bursts inflates p99 badly. Use requests for scheduling and let it
  burst.
- **`-memory-budget-mb`** should be under the container's memory limit with
  room for the render working set - roughly half. The registry refuses a load
  that would exceed it rather than overshooting.

## Timeouts and draining

```yaml
terminationGracePeriodSeconds: 45        # > the longest chunk, < the ingress timeout
```

The service already does graceful shutdown: it stops accepting, lets in-flight
renders finish, and exits. With chunks of 200 (~6 s) a 45 s grace period drains
comfortably. Add a `PodDisruptionBudget` with `minAvailable` at ~50% so a node
drain cannot take the whole deployment at once.

Set `-timeout` (default 10 minutes) below your ingress read timeout, so a
request that is going to be cut off is cut off by the service - with a
response - rather than by the proxy, with a connection reset.

## Responses

The bulk endpoint streams NDJSON: one line per customer as it completes, then a
summary line. Three consequences worth knowing.

**It is not a JSON array, on purpose.** Buffering 4000 results to emit one
document means holding them all in memory and sending nothing until the slowest
customer finishes. Streaming keeps memory proportional to concurrency: 11.1 GB
of manifests moved through a process holding 925 MB.

**Time-to-first-result is ~600 ms regardless of batch size.** A client can show
progress, and a failing config surfaces immediately instead of after the batch.

**Verdicts, not payloads, by default.** Each result is a manifest count, a byte
count and a content digest. The digest is the useful part - it is how you tell
that a customer's output changed without diffing 10 MB of YAML. `?manifests=true`
attaches the rendered output and is for single renders; asking for it across a
bulk batch is how you turn a 1 MB response into 11 GB.

If you need the manifests for a batch, have the service write them to object
storage and return keys. Do not stream gigabytes through an ingress controller.

## When to add a job API

The design above is a synchronous API with client-side batching. Move to
submit/poll when you need something it cannot express:

- results that outlive the client connection
- resumability across a client restart
- batches large enough that even chunked they run for many minutes
- per-tenant fairness or prioritisation between batches

The building blocks are already right for it: results are per-config and
independent, chunks are idempotent, and a chunk is a natural unit of work for a
queue. That is a deliberate property, not a coincidence - but it is not a
reason to build the queue before something needs it.

## Reference manifest

```yaml
apiVersion: apps/v1
kind: Deployment
metadata: { name: hedp }
spec:
  replicas: 6
  selector: { matchLabels: { app: hedp } }
  template:
    metadata: { labels: { app: hedp } }
    spec:
      terminationGracePeriodSeconds: 45
      containers:
        - name: hedp
          image: registry.internal/hedp:1.0.0
          args:
            - -cached-engine
            - -bundle-url=https://artifacts.internal/hedp/{revision}.tar
            - -revision=$(LIBRARY_REVISION)
            - -max-in-flight=2
            - -memory-budget-mb=1500
            - -release-concurrency=1        # bulk-tuned; see below
            - -timeout=2m
          env:
            - { name: LIBRARY_REVISION, value: "9f3c1ab" }
            - { name: GOGC,             value: "400" }
            - { name: GOMEMLIMIT,       value: "3500MiB" }
            - { name: GOMAXPROCS,       value: "4" }
          ports: [{ containerPort: 8080 }]
          resources:
            requests: { cpu: "4", memory: "3Gi" }
            limits:   { memory: "4Gi" }
          livenessProbe:
            httpGet: { path: /healthz, port: 8080 }
          readinessProbe:
            httpGet: { path: /readyz, port: 8080 }
            initialDelaySeconds: 2
            periodSeconds: 5
```

### One deployment or two?

`-release-concurrency` trades single-request latency against bulk throughput,
and one process cannot do both:

- `1` - a customer's releases render serially. Bulk-tuned. A single render is
  ~112 ms.
- `4` - a customer's releases render in parallel. Latency-tuned. A single
  render is ~40 ms, but each request occupies every core.

If you serve both interactive single renders and bulk batches, run **two
deployments off the same image** behind different Services, rather than
compromising in one. They share nothing but the artifact store.
