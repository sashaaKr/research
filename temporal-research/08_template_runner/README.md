# 08 — Template Runner

A generic workflow interpreter that executes any `Template` definition and
exposes a complete execution graph — including nodes that were **skipped**.

## Architecture

```
Template (data)         TemplateRunnerWorkflow         Live Visualiser
─────────────           ──────────────────────         ────────────────
nodes + conditions  ──▶  interpreter (generic)  ◀───  get_execution_graph()
contexts (dev/…)         dispatches by name            polls every 400ms
                         tracks ALL node states        renders full graph
```

## What makes this different from example 07

| | 07_provisioning | 08_template_runner |
|---|---|---|
| Workflow shape | Hard-coded in Python | Driven by a data template |
| Skipped nodes visible | No | **Yes — always in graph** |
| Condition visibility | Implicit (absent from history) | **Explicit SKIPPED + reason** |
| Add a new step | Edit `workflows.py` + redeploy | Edit template data only |
| Activity dispatch | Direct function reference | By string name |

## Execution model

Every node is launched as a concurrent coroutine immediately.
Each coroutine waits (`workflow.wait_condition`) for its declared dependencies
to reach a terminal state, then evaluates its condition:

```
for every node, concurrently:
  wait until all depends_on are terminal (completed / skipped / failed)
  │
  ├─ condition false?          → SKIPPED  (reason: "condition 'x' = false")
  ├─ any dep was skipped?      → SKIPPED  (reason: "upstream 'y' was skipped")
  ├─ any dep failed?           → SKIPPED  (reason: "upstream 'y' failed")
  └─ otherwise                 → RUNNING → COMPLETED | FAILED
```

Cascading skips propagate automatically: if `server_2` is SKIPPED,
then `load_balancer` (which depends on `server_2`) is also SKIPPED,
and so is `health_lb` (which depends on `load_balancer`).

## Param references

Node params support `${source.field}` interpolation:

```python
params={
    "vpc_id":      "${vpc.vpc_id}",          # output of completed node 'vpc'
    "region":      "${ctx.region}",          # input context field
    "instance_type": "${ctx.instance_type}",
}
```

## Three scenarios

```
dev      1 server │ no DB │ no cache │ no LB
                  │ multi_server, include_database, include_cache,
                  │ include_load_balancer all absent → SKIPPED

staging  2 servers │ DB ✓ │ no cache │ LB ✓
                   │ include_cache absent → cache + health_cache SKIPPED

prod     3 servers │ DB ✓ │ cache ✓  │ LB ✓
                   │ all nodes run
```

## How to run

```bash
# Terminal 1
python worker.py

# Terminal 2
python starter.py dev
python starter.py staging
python starter.py prod
python starter.py prod --cancel   # sends cancel signal after 1.5s
```

## Example output (staging)

```
Cloud Infrastructure Provisioning  [staging]
────────────────────────────────────────────────────────────────────────
  ✓ Provision VPC                             COMPLETED    vpc_id=vpc-3f2a1b4c
  │
  ✓ Subnet A (az-a)                           COMPLETED    subnet_id=subnet-11
  ✓ Subnet B (az-b)                           COMPLETED    subnet_id=subnet-22
  ✓ Subnet C (az-c)                           COMPLETED    subnet_id=subnet-33
  │
  ✓ Server 1                                  COMPLETED    server_id=i-aabbcc
  ✓ Server 2                [if multi_server] COMPLETED    server_id=i-ddeeff
  ⊘ Server 3                [if multi_server] SKIPPED      ↳ condition 'multi_server' = false
  ✓ RDS Database            [if include_db]   COMPLETED    db_id=db-112233
  ⊘ ElastiCache Redis       [if include_cache] SKIPPED     ↳ condition 'include_cache' = false
  │
  ✓ Application Load Balancer [if incl. lb]   COMPLETED    lb_id=alb-99aabb
  │
  ✓ Register DNS Records                      COMPLETED    zone=staging.internal
  │
  ✓ Health: Server 1                          COMPLETED    healthy=True
  ✓ Health: Server 2        [if multi_server] COMPLETED    healthy=True
  ⊘ Health: Server 3        [if multi_server] SKIPPED      ↳ upstream 'server_3' was skipped
  ✓ Health: Database        [if include_db]   COMPLETED    healthy=True
  ⊘ Health: Cache           [if include_cache] SKIPPED     ↳ condition 'include_cache' = false
  ✓ Health: Load Balancer   [if incl. lb]     COMPLETED    healthy=True
────────────────────────────────────────────────────────────────────────
COMPLETED 10  SKIPPED 6
```

Every declared node is visible. Skipped nodes show exactly why they were skipped.
