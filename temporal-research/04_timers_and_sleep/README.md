# 04 — Timers & Sleep

Durable timers: how workflows pause for minutes, hours, or days without
holding any server resources.

## The core insight

In Temporal, `workflow.wait_condition(timeout=timedelta(days=30))` persists the
timer in the **server** as a single event. The worker process can be shut down,
redeployed, or crash — the timer fires at exactly the right wall-clock time
regardless, and the workflow resumes on whatever worker is available then.

This is fundamentally different from:
- `asyncio.sleep()` — lost if the process restarts
- A cron job + database — requires external coordination
- A message queue delay — no durable state, hard to correlate

## Escalation pattern

```
Ticket opened
     │
     ▼  wait 1h (20s in demo)
 ┌─────────────────────────────────────────────────────┐
 │ Resolved? ──yes──▶ Close ticket                     │
 │    no                                               │
 └─────────────────────────────────────────────────────┘
     │
     ▼ send reminder, wait another 1h
 ┌─────────────────────────────────────────────────────┐
 │ Resolved? ──yes──▶ Close ticket                     │
 │    no                                               │
 └─────────────────────────────────────────────────────┘
     │
     ▼ escalate to manager, wait indefinitely
     ▼ resolve signal → close ticket
```

## How to run

```bash
# Terminal 1
python worker.py

# Terminal 2: let timers fire naturally (watch escalation in worker logs)
python starter.py TKT-001

# Terminal 2 (alternative): resolve early via signal
python starter.py TKT-002 --resolve
```

## What to observe

- Without `--resolve`: watch the worker print `[REMINDER]` after 20s,
  then `[ESCALATION]` after another 20s. The workflow is paused the whole
  time, consuming zero CPU.
- With `--resolve`: the in-progress timer is cancelled and the workflow
  completes cleanly.

## Timer cost

Temporal timers are extremely cheap. You can have millions of workflows each
sleeping for days — the server just stores a scheduled-fire timestamp. No
polling, no threads, no memory allocation until the timer fires.
