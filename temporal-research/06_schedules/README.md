# 06 — Schedules

Run workflows on a recurring basis without cron daemons or external schedulers.

## What is a Temporal Schedule?

A **Schedule** is a server-side object that:
1. Holds a spec (interval or cron expression)
2. Knows which workflow to launch and with what arguments
3. Tracks history of recent runs and upcoming trigger times
4. Can be paused, unpaused, triggered manually, and backfilled

Each trigger creates a fresh, independent workflow execution. The schedule and
the workflow executions are separate — deleting a schedule does not affect
already-running workflows.

## Overlap policy

What should happen if the previous run hasn't finished when the next trigger fires?

| Policy | Behaviour |
|---|---|
| `SKIP` | Skip this trigger (default — safe for idempotent jobs) |
| `BUFFER_ONE` | Queue one extra run |
| `BUFFER_ALL` | Queue all missed runs |
| `CANCEL_OTHER` | Cancel the running execution and start a new one |
| `ALLOW_ALL` | Always start a new execution regardless |

## How to run

```bash
# Terminal 1 — worker
python worker.py

# Terminal 2 — create the schedule
python manage_schedule.py create

# Wait ~60s and watch the worker print reports.

python manage_schedule.py describe   # inspect schedule state
python manage_schedule.py trigger    # fire an immediate run
python manage_schedule.py pause      # stop future triggers
python manage_schedule.py unpause    # resume
python manage_schedule.py delete     # clean up
```

## Cron syntax

Instead of `intervals`, you can use standard cron expressions:

```python
spec=ScheduleSpec(
    cron_expressions=["0 8 * * MON-FRI"],  # weekdays at 08:00
)
```

The spec also supports `jitter` (add random offset to prevent thundering herds)
and `timezone` for time-zone-aware firing times.

## Backfill

If a schedule was paused or the worker was down, you can backfill missed runs:

```python
from temporalio.client import ScheduleBackfill
handle = client.get_schedule_handle(SCHEDULE_ID)
await handle.backfill([
    ScheduleBackfill(start_at=start, end_at=end, overlap=ScheduleOverlapPolicy.ALLOW_ALL)
])
```

Temporal will immediately trigger executions for all missed intervals in the
specified range, letting you recover lost data pipeline runs, report generations, etc.
