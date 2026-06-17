# 07 — Complex Provisioning Flow

A realistic multi-phase workflow with parallelism, conditional branches,
child workflows, live progress queries, and a cancellation signal.

## Execution plan

```
EnvironmentProvisioningWorkflow(req)
│
├─ Phase 1: Network
│    provision_vpc()                          ← sequential (subnets need the VPC ID)
│    ├── provision_subnet(az-a)  ─┐
│    ├── provision_subnet(az-b)  ─┤ parallel (asyncio.gather)
│    └── provision_subnet(az-c)  ─┘
│                  ↓ cancel checkpoint
│
├─ Phase 2: Compute  ──────────────────────── all three groups run concurrently
│    ├── ServerProvisioningWorkflow(server-0) ─┐
│    ├── ServerProvisioningWorkflow(server-1) ─┤ parallel child workflows
│    ├── ServerProvisioningWorkflow(server-N) ─┘
│    │
│    ├── provision_database()   [if include_database]   ← conditional
│    └── provision_cache()      [if include_cache]      ← conditional
│                  ↓ cancel checkpoint
│
├─ Phase 3: Load Balancer
│    └── provision_load_balancer()  [only if server_count > 1]  ← conditional
│
├─ Phase 4: DNS
│    └── register_dns_records([...all endpoints...])    ← single batched call
│
└─ Phase 5: Health Checks
     ├── run_health_check(server-0) ─┐
     ├── run_health_check(server-N) ─┤ parallel
     ├── run_health_check(lb)       ─┤ (if provisioned)
     ├── run_health_check(db)       ─┤ (if provisioned)
     └── run_health_check(cache)   ─┘ (if provisioned)
```

## Three scenarios

| Scenario | Servers | DB | Cache | LB |
|---|---|---|---|---|
| `dev` | 1 | ✗ | ✗ | ✗ (no LB for single server) |
| `staging` | 2 | ✓ | ✗ | ✓ |
| `prod` | 4 | ✓ | ✓ | ✓ |

## Key patterns demonstrated

### Parallel execution (`asyncio.gather`)
Subnets, servers, health checks all use `asyncio.gather` so independent
work runs concurrently. Phase 2 nests two levels: all server child workflows
run in parallel *and* in parallel with DB/cache provisioning.

### Conditional branches
`maybe_db()` and `maybe_cache()` are local async functions that either call
an activity or return `None` immediately — clean conditional branching
without if/else sprawl at the gather site.

### Child workflows (fan-out)
Each server is its own `ServerProvisioningWorkflow` execution with a unique
ID (`{workflow_id}-server-{i}`). Each child is independently observable in
the Web UI and has its own retry scope. If server-2 fails, Temporal retries
only server-2.

### Soft cancellation via signal
A `cancel_provisioning` signal sets a flag. The workflow checks the flag at
defined checkpoints (between phases) rather than mid-activity, giving
activities time to complete cleanly.

### Live progress via query
`starter.py` polls `get_progress()` every 500ms and renders the current
phase and resource inventory while the workflow runs.

## How to run

```bash
# Terminal 1
python worker.py

# Terminal 2
python starter.py dev
python starter.py staging
python starter.py prod

# Cancel partway through (sends signal after 2s)
python starter.py prod --cancel
```

Open the Web UI and inspect `provision-prod`:
- The parent workflow shows all 5 phases in its event history.
- Each `provision-prod-server-{i}` is a separate child workflow execution.
- Phase 2 activities (db, cache) run concurrently alongside the child workflows.
