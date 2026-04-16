from datetime import timedelta

from temporalio import workflow

# Guard the activity import so the workflow sandbox doesn't execute it at
# import time. Workflows run in a restricted environment to enforce
# determinism — this pattern lets you import normal Python code safely.
with workflow.unsafe.imports_allowed():
    from activities import say_hello


@workflow.defn
class HelloWorldWorkflow:
    """
    The simplest possible workflow: call one activity, return the result.

    Key concepts shown here:
    - @workflow.defn  marks a class as a Temporal workflow definition.
    - @workflow.run   marks the entry point (must be async, exactly one per class).
    - workflow.execute_activity() schedules an activity on the task queue.
      The workflow is *suspended* (but durably persisted) while waiting.
    - start_to_close_timeout: max wall-clock time allowed for the activity.

    Determinism rule: workflow code must be deterministic across replays.
    No random, no datetime.now(), no direct network calls — those belong
    in activities.
    """

    @workflow.run
    async def run(self, name: str) -> str:
        return await workflow.execute_activity(
            say_hello,
            name,
            start_to_close_timeout=timedelta(seconds=10),
        )
