# 02 — Retry & Errors

How Temporal handles failures automatically — and how you control that behaviour.

## Scenarios

### `flaky` — Custom RetryPolicy with exponential backoff

The activity fails the first 3 attempts then succeeds. The custom `RetryPolicy`
configures exponential backoff (1s → 2s → 4s) capped at 5 total attempts.

Watch the attempt counter climb in the worker logs and the Web UI event history.

### `non_retryable` — Immediate failure, no retries

The activity raises `ApplicationError(non_retryable=True)`. Temporal propagates
this straight to the workflow without retrying. Use this for:
- Invalid input that can't be fixed by retrying
- "Not found" / "Already exists" business errors
- Anything where the same call will always fail

### `heartbeat` — Long-running activity with heartbeat detection

A 5-step task sends a heartbeat after each step. The `heartbeat_timeout`
ensures Temporal detects a crashed worker quickly and reschedules the activity.
The heartbeat payload (`{"step": N, "total": M}`) lets the replacement worker
resume mid-task rather than restarting.

## Default retry behaviour

When you *don't* specify a `RetryPolicy`, Temporal uses:

| Setting | Default |
|---|---|
| initial_interval | 1 second |
| backoff_coefficient | 2.0 |
| maximum_interval | 100 × initial_interval |
| maximum_attempts | unlimited |
| non_retryable_error_types | `[]` |

Activities retry indefinitely by default — always set `maximum_attempts` or a
workflow-level `start_to_close_timeout` so executions don't run forever.

## How to run

```bash
# Terminal 1
python worker.py

# Terminal 2
python starter.py flaky
python starter.py non_retryable
python starter.py heartbeat
```

## What to look for in the UI

- **flaky**: Multiple `ActivityTaskFailed` events before `ActivityTaskCompleted`.
- **non_retryable**: A single `ActivityTaskFailed` then `WorkflowExecutionFailed`.
- **heartbeat**: Smooth `ActivityTaskCompleted` after several heartbeat ticks.
