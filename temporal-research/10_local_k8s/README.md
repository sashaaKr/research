# 10 — Local Kubernetes Setup

Fully local end-to-end: Temporal server in Docker Compose, workers deployed in Kind.
One command spins everything up; another tears it down.

## Architecture

```
┌─────────────────────────────────── docker-compose (temporal-net) ──────────────┐
│  postgresql:5432   temporal:7233   temporal-ui:8080                            │
└───────────────────────────────────────────────┬────────────────────────────────┘
                                                │  docker network connect
              ┌─────────────────────────────────┴──────────────────────────────┐
              │  Kind cluster (temporal-local)  ← nodes joined to temporal-net │
              │                                                                  │
              │  Deployment: temporal-orchestrator (2 pods, fixed)              │
              │  Deployment: temporal-activities   (2–6 pods, HPA)              │
              │                                                                  │
              │  Pods resolve "temporal:7233" via the shared Docker network     │
              └──────────────────────────────────────────────────────────────────┘
```

The networking trick: after creating the Kind cluster, `make cluster` runs
`docker network connect temporal-net <node>` for every Kind node. This gives
pods inside the cluster direct access to docker-compose services by their
service name (`temporal`, `postgresql`, etc.) without any port-forwarding.

## Prerequisites

```
kind    — https://kind.sigs.k8s.io/docs/user/quick-start/#installation
kubectl — https://kubernetes.io/docs/tasks/tools/
docker  — running daemon
uv      — https://docs.astral.sh/uv/getting-started/installation/
```

## Usage

```bash
# Full setup (5–10 min first run — pulls images, builds, loads into Kind)
make setup

# Open Temporal UI
open http://localhost:8080

# Submit a test workflow and watch it in the terminal visualiser
make run-workflow

# Tail worker logs
make logs-orchestrator
make logs-activities

# After changing Python code — rebuild and rolling-restart workers
make redeploy

# Enable HPA (requires metrics-server)
make metrics-server

# Tear everything down
make teardown
```

## File layout

```
10_local_k8s/
├── Makefile               orchestrates all steps
├── kind-config.yaml       1 control-plane + 2 worker nodes
└── k8s/
    ├── namespace.yaml
    ├── configmap.yaml     TEMPORAL_HOST=temporal:7233 (docker-compose service)
    ├── orchestrator-deployment.yaml   image: temporal-worker:local, Never pull
    ├── activities-deployment.yaml     image: temporal-worker:local, Never pull
    ├── activities-hpa.yaml            CPU-based, min 2 / max 6 (laptop-sized)
    └── pdb.yaml
```

## How it differs from 09_k8s

| | 09_k8s | 10_local_k8s |
|---|---|---|
| Image | `your-registry/temporal-worker:latest` | `temporal-worker:local` (Never pull) |
| Temporal host | in-cluster service or Cloud endpoint | `temporal:7233` (docker-compose) |
| Resources | production-sized | scaled down for a laptop |
| HPA max | 20 replicas | 6 replicas |
| Teardown | manual | `make teardown` |

## Troubleshooting

**Pods stuck in `Pending`**
```bash
kubectl describe pod -n temporal-workers --context kind-temporal-local
```
Usually a topology spread issue — check that both worker nodes are `Ready`.

**Pods can't reach `temporal:7233`**
```bash
# Verify Kind nodes are on the docker-compose network
docker network inspect temporal-net | grep temporal-local
```
If missing, re-run: `make cluster`

**Image not found (`ErrImageNeverPull`)**
```bash
make build load
```
