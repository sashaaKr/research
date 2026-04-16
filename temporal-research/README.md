# Temporal Framework Research

Progressive examples for understanding Temporal — a durable execution platform
for building reliable, long-running workflows.

## What is Temporal?

Temporal separates **workflow logic** (what should happen and in what order) from
**execution infrastructure** (retries, timeouts, state persistence). Your code
describes the process; Temporal guarantees it runs to completion even if workers
crash, networks fail, or the server restarts.

Core concepts:

| Concept | Description |
|---|---|
| **Workflow** | A durable function that orchestrates work. Code is replayed on recovery — must be deterministic. |
| **Activity** | A regular function that does the actual work (API calls, DB, I/O). Can be non-deterministic. |
| **Worker** | A process that polls a task queue and executes workflows/activities. |
| **Task Queue** | The named channel connecting clients → server → workers. |
| **Signal** | An async message pushed INTO a running workflow from outside. |
| **Query** | A synchronous read of a running workflow's state. |
| **Schedule** | A cron-like mechanism for launching workflows on a recurring basis. |

## Examples

| # | Topic | Concepts |
|---|---|---|
| 01 | [Hello World](./01_hello_world/) | Workflow, Activity, Worker, Client |
| 02 | [Retry & Errors](./02_retry_and_errors/) | RetryPolicy, non-retryable errors, heartbeating |
| 03 | [Signals & Queries](./03_signals_and_queries/) | `@workflow.signal`, `@workflow.query`, `wait_condition` |
| 04 | [Timers & Sleep](./04_timers_and_sleep/) | Durable timers, escalation pattern |
| 05 | [Child Workflows](./05_child_workflows/) | Parallel child workflows, `asyncio.gather` |
| 06 | [Schedules](./06_schedules/) | Create/pause/trigger/delete schedules |

## Setup

### Option A — Temporal CLI dev server (recommended for local dev)

```bash
# macOS
brew install temporal

# Linux / other: https://docs.temporal.io/cli#installation
curl -sSf https://temporal.download/cli.sh | sh

# Start the dev server (gRPC :7233, Web UI :8233)
temporal server start-dev
```

### Option B — Docker Compose (closer to production)

```bash
docker compose up -d
# Web UI → http://localhost:8080
```

### Python environment

```bash
cd temporal-research
python -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt
```

## Running each example

Each example is self-contained. Open two terminals inside the example directory:

```bash
# Terminal 1 — start the worker
cd temporal-research/01_hello_world
python worker.py

# Terminal 2 — start a workflow execution
python starter.py
```
