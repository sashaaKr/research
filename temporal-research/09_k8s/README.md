# 09 — Kubernetes Deployment

How to deploy Temporal workers on Kubernetes using two separate Deployments
sized and scaled independently.

## Architecture

```
                       ┌──────────────────────────────────────────────┐
                       │  Temporal Server (in-cluster or Cloud)       │
                       │                                              │
                       │  task queue: "template-runner-queue"  ←──┐  │
                       │  task queue: "provisioning-queue"     ←─┐│  │
                       └──────────────────────────────────────────┼┼──┘
                                                                   ││
                ┌──────────────────────────────┐    ┌─────────────┼┼──────────────────────────┐
                │  Deployment: orchestrator    │    │  Deployment: activities                  │
                │  replicas: 2 (fixed)         │    │  replicas: 3–20 (HPA)                   │
                │  cpu: 100m–500m              │    │  cpu: 500m–2                            │
                │  memory: 128–256Mi           │    │  memory: 512Mi–2Gi                      │
                │                              │    │                                          │
                │  WORKER_TYPE=orchestrator    │    │  WORKER_TYPE=activities                  │
                │  TASK_QUEUE=                 │    │  TASK_QUEUE=                             │
                │    template-runner-queue  ───┘    │    provisioning-queue  ──────────────────┘
                │  ACTIVITY_TASK_QUEUE=             │
                │    provisioning-queue             │
                └──────────────────────────────────┘
                  registers: TemplateRunnerWorkflow    registers: provision_vpc, provision_subnet,
                  (orchestration only)                            provision_server, provision_database,
                                                                  provision_cache, provision_lb,
                                                                  register_dns, run_health_check
```

### Why two Deployments?

| | Orchestrator | Activities |
|---|---|---|
| CPU per pod | ~100m (workflow replay is fast) | 500m–2 (real I/O / compute) |
| Memory | ~128Mi | 512Mi–2Gi |
| Scaling | Fixed 2 replicas (HA) | HPA: 2–20 replicas |
| Crash impact | Temporal reschedules workflow task | Temporal retries activity |
| Rolling deploy | Zero-downtime, workflow resumes | Graceful drain, in-flight activities complete |

### One image, two roles

The same Docker image runs both Deployments. `WORKER_TYPE` selects what gets registered:

```
WORKER_TYPE=orchestrator  → registers TemplateRunnerWorkflow (workflow tasks)
WORKER_TYPE=activities    → registers all activity functions  (activity tasks)
WORKER_TYPE=all           → registers both (default — use for local dev)
```

## Files

```
09_k8s/
├── app/
│   ├── Dockerfile               built from temporal-research/ as context
│   ├── pyproject.toml
│   ├── worker.py                entry point — all config via env vars
│   └── interpreter.py           extended from 08: adds activity_task_queue routing
└── k8s/
    ├── namespace.yaml
    ├── configmap.yaml           Temporal host, queue names, concurrency tuning
    ├── secret.yaml              template for Temporal Cloud TLS credentials
    ├── orchestrator-deployment.yaml
    ├── activities-deployment.yaml
    ├── activities-hpa.yaml      CPU-based HPA + KEDA alternative in comments
    └── pdb.yaml                 PodDisruptionBudgets for both Deployments
```

## Build & deploy

```bash
# 1. Build the image (from temporal-research/ as context)
docker build \
  -f 09_k8s/app/Dockerfile \
  -t your-registry/temporal-worker:latest \
  .

docker push your-registry/temporal-worker:latest

# 2. Update image name in the Deployment manifests, then apply
kubectl apply -f 09_k8s/k8s/namespace.yaml
kubectl apply -f 09_k8s/k8s/configmap.yaml
kubectl apply -f 09_k8s/k8s/orchestrator-deployment.yaml
kubectl apply -f 09_k8s/k8s/activities-deployment.yaml
kubectl apply -f 09_k8s/k8s/activities-hpa.yaml
kubectl apply -f 09_k8s/k8s/pdb.yaml

# 3. Verify
kubectl get pods -n temporal-workers
kubectl logs -n temporal-workers -l app=temporal-orchestrator -f
kubectl logs -n temporal-workers -l app=temporal-activities   -f
```

## Temporal Cloud

Swap the ConfigMap host for your Cloud endpoint and mount the TLS Secret:

```bash
# Create the secret from your Cloud cert files
kubectl create secret generic temporal-cloud-tls \
  --from-file=TEMPORAL_TLS_CERT=client.pem \
  --from-file=TEMPORAL_TLS_KEY=client.key  \
  -n temporal-workers

# Then uncomment the secretKeyRef blocks in both Deployment manifests
```

## Graceful shutdown & rolling deployments

Temporal workers handle `SIGTERM` by:
1. Stopping polling (no new tasks accepted)
2. Waiting up to `GRACEFUL_SHUTDOWN_SECS` for in-flight tasks to finish
3. Exiting cleanly

K8s sends `SIGTERM` and waits `terminationGracePeriodSeconds` before `SIGKILL`.
The manifests set `terminationGracePeriodSeconds: 60` and `GRACEFUL_SHUTDOWN_SECS: 45`,
giving a 15s margin. Adjust both together based on your longest activity duration.

## Scaling

### Activities (HPA)
The HPA scales the activities Deployment between 2 and 20 pods based on CPU.
At 60% CPU utilisation per pod it adds up to 4 pods per minute.
Scale-down is deliberately slow (5-minute window) to avoid terminating pods
that have in-flight activities.

### KEDA (recommended for production)
The HPA comment in `activities-hpa.yaml` shows how to use KEDA's Temporal
scaler instead — it drives scaling from the actual task queue backlog depth,
which is more accurate than CPU and reacts faster to bursts.

### Orchestrator (fixed)
The orchestrator Deployment uses a fixed replica count of 2. Workflow task
processing is fast and mostly idle (waiting for activities) so CPU rarely
spikes. If you have thousands of concurrent workflows, increase this manually.
