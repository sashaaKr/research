# 03 — Signals & Queries

How external code communicates with a *running* workflow.

## The problem

Long-running workflows need to react to external events (a human clicks
"Approve", a payment gateway posts a webhook, a timeout fires). They also need
to expose their state without storing it in a separate database.

Temporal solves both with **signals** and **queries**.

## Signals vs Queries

| | Signal | Query |
|---|---|---|
| Direction | INTO the workflow | OUT of the workflow |
| Execution | Async (fire and forget from caller) | Sync (returns immediately) |
| Side-effects | Yes — can modify workflow state | No — must be pure reads |
| Persistence | Yes — replayed on recovery | No — reads live in-memory state |
| Use for | Approvals, events, cancellations | Dashboards, status checks |

## How to run

```bash
# Terminal 1 — worker
python worker.py

# Terminal 2 — start the workflow (will block waiting for a signal)
python starter.py ORD-007 2500

# Terminal 3 — interact with the running workflow
python interact.py order-ORD-007 status
python interact.py order-ORD-007 approve
# OR
python interact.py order-ORD-007 reject "Over budget"
```

## `wait_condition` — the key primitive

```python
await workflow.wait_condition(
    lambda: self._approved is not None,
    timeout=timedelta(hours=24),
)
```

This suspends the workflow coroutine durably. The predicate is re-evaluated
after every signal delivery. If the timeout fires first, `asyncio.TimeoutError`
is raised.

Unlike `asyncio.sleep()`, `wait_condition` survives worker restarts — Temporal
replays the event history and the workflow ends up in exactly the same suspended
state.

## Signal-with-Start

A powerful pattern not shown here: `client.start_workflow()` with
`start_signal=` and `start_signal_args=`. If the workflow isn't running yet,
this starts it *and* immediately delivers the signal in a single atomic
operation. Useful for event-driven architectures where the trigger and the
workflow creation happen together.
