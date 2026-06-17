"""
Manage the MetricsReport schedule.

Commands:
    python manage_schedule.py create     -- create schedule (runs every minute)
    python manage_schedule.py list       -- list all schedules in the namespace
    python manage_schedule.py describe   -- show details of this schedule
    python manage_schedule.py trigger    -- trigger an immediate run
    python manage_schedule.py pause      -- pause (stop future runs)
    python manage_schedule.py unpause    -- resume paused schedule
    python manage_schedule.py delete     -- delete the schedule permanently
"""
import asyncio
import sys
from datetime import timedelta

from temporalio.client import (
    Client,
    Schedule,
    ScheduleActionStartWorkflow,
    ScheduleIntervalSpec,
    ScheduleOverlapPolicy,
    ScheduleSpec,
    ScheduleState,
)

from workflows import MetricsReportWorkflow

SCHEDULE_ID = "metrics-report-schedule"
TASK_QUEUE = "schedules-queue"


async def cmd_create(client: Client) -> None:
    await client.create_schedule(
        SCHEDULE_ID,
        Schedule(
            action=ScheduleActionStartWorkflow(
                MetricsReportWorkflow.run,
                "production",                  # workflow argument
                id=f"metrics-report-{{scheduledTime}}",   # unique ID per trigger
                task_queue=TASK_QUEUE,
            ),
            spec=ScheduleSpec(
                # Run every 60 seconds (use timedelta(hours=24) for a daily report)
                intervals=[ScheduleIntervalSpec(every=timedelta(seconds=60))],
                # Alternative: cron_expressions=["0 8 * * MON-FRI"]
            ),
            # SKIP: if a previous run is still in progress, skip this trigger.
            # Other options: BUFFER_ONE, BUFFER_ALL, CANCEL_OTHER, ALLOW_ALL.
            overlap=ScheduleOverlapPolicy.SKIP,
            state=ScheduleState(note="Created by manage_schedule.py"),
        ),
    )
    print(f"Schedule {SCHEDULE_ID!r} created. It will fire every 60 seconds.")
    print("Watch the worker terminal or the Web UI → Schedules.")


async def cmd_list(client: Client) -> None:
    print("All schedules in this namespace:")
    async for entry in await client.list_schedules():
        paused = entry.schedule.state.paused if entry.schedule.state else False
        print(f"  {entry.id}  [{'PAUSED' if paused else 'active'}]")


async def cmd_describe(client: Client) -> None:
    handle = client.get_schedule_handle(SCHEDULE_ID)
    desc = await handle.describe()
    print(f"ID         : {desc.id}")
    print(f"Spec       : {desc.schedule.spec}")
    print(f"Overlap    : {desc.schedule.overlap}")
    print(f"Paused     : {desc.schedule.state.paused}")
    print(f"Note       : {desc.schedule.state.note}")
    print(f"Next runs  : {desc.info.next_action_times[:3]}")
    print(f"Recent runs: {[str(r.scheduled_time) for r in desc.info.recent_actions[-3:]]}")


async def cmd_trigger(client: Client) -> None:
    handle = client.get_schedule_handle(SCHEDULE_ID)
    await handle.trigger()
    print(f"Triggered an immediate run of {SCHEDULE_ID!r}.")


async def cmd_pause(client: Client) -> None:
    handle = client.get_schedule_handle(SCHEDULE_ID)
    await handle.pause(note="Paused via manage_schedule.py")
    print(f"Schedule {SCHEDULE_ID!r} paused. Future triggers are suppressed.")


async def cmd_unpause(client: Client) -> None:
    handle = client.get_schedule_handle(SCHEDULE_ID)
    await handle.unpause(note="Unpaused via manage_schedule.py")
    print(f"Schedule {SCHEDULE_ID!r} unpaused. Triggers will resume.")


async def cmd_delete(client: Client) -> None:
    handle = client.get_schedule_handle(SCHEDULE_ID)
    await handle.delete()
    print(f"Schedule {SCHEDULE_ID!r} deleted.")


COMMANDS = {
    "create": cmd_create,
    "list": cmd_list,
    "describe": cmd_describe,
    "trigger": cmd_trigger,
    "pause": cmd_pause,
    "unpause": cmd_unpause,
    "delete": cmd_delete,
}


async def main(command: str) -> None:
    if command not in COMMANDS:
        print(f"Unknown command: {command!r}")
        print(f"Available: {', '.join(COMMANDS)}")
        sys.exit(1)

    client = await Client.connect("localhost:7233")
    await COMMANDS[command](client)


if __name__ == "__main__":
    command = sys.argv[1] if len(sys.argv) > 1 else "create"
    asyncio.run(main(command))
