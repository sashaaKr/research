# 01 — Hello World

The minimum viable Temporal program: one workflow, one activity.

## Architecture

```
starter.py ──────────▶ Temporal Server ◀────────── worker.py
(submit + wait)         (stores state,               (polls, executes
                         schedules tasks)             workflows + activities)
```

The client (`starter.py`) and the worker are completely decoupled — they only
communicate through the Temporal server. Workers can be restarted, scaled, or
replaced without the client caring.

## Files

| File | Role |
|---|---|
| `activities.py` | `say_hello` — the actual work |
| `workflows.py` | `HelloWorldWorkflow` — the orchestration |
| `worker.py` | Registers workflows + activities, polls task queue |
| `starter.py` | Submits the workflow, waits for the result |

## How to run

```bash
# Terminal 1 — worker (keep running)
cd temporal-research/01_hello_world
python worker.py

# Terminal 2 — start the workflow
python starter.py
# → Workflow result: 'Hello, World!'
```

## What to observe in the UI

1. Open the Web UI (`:8233` for `temporal server start-dev`, `:8080` for Docker).
2. Find workflow ID `hello-world-01`.
3. The **Event History** shows every step: `WorkflowExecutionStarted` →
   `ActivityTaskScheduled` → `ActivityTaskCompleted` → `WorkflowExecutionCompleted`.
4. This event log is the source of truth — Temporal replays it to recover
   workflows after crashes.

## Key takeaways

- Workflows **orchestrate**; activities **execute**. Keep them separate.
- `start_to_close_timeout` is required — Temporal won't infer a default.
- The workflow `id` is your handle to the execution. Use a meaningful, stable ID
  (e.g., `order-{order_id}`) so your business logic can reference it later.
