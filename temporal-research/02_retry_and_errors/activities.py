import asyncio
import logging

from temporalio import activity
from temporalio.exceptions import ApplicationError

logger = logging.getLogger(__name__)


@activity.defn
async def flaky_service_call(fail_first_n: int) -> str:
    """
    Simulates a flaky external service that fails the first N attempts.

    activity.info().attempt is 1-based. Temporal increments it automatically
    on each retry so you can implement back-off logic or skip already-done work.
    """
    info = activity.info()
    logger.info("flaky_service_call attempt #%d", info.attempt)

    if info.attempt <= fail_first_n:
        # Default ApplicationError is retryable — Temporal will retry it
        # according to the RetryPolicy configured on the activity call.
        raise ApplicationError(
            f"Service unavailable (attempt {info.attempt}/{fail_first_n})"
        )

    return f"Success on attempt #{info.attempt}!"


@activity.defn
async def validate_business_rule(value: int) -> str:
    """
    Simulates a business rule that must NOT be retried on failure.

    Use non_retryable=True when retrying is pointless — bad input, constraint
    violations, "item not found", etc. Temporal will fail the workflow
    immediately instead of burning retry attempts.
    """
    logger.info("validate_business_rule called with value=%d", value)

    if value < 0:
        raise ApplicationError(
            f"Value must be non-negative, got {value}",
            non_retryable=True,  # ← workflow fails immediately, no retries
        )

    return f"Value {value} passed validation"


@activity.defn
async def long_running_task(steps: int) -> str:
    """
    Simulates a long-running activity (large file processing, ML inference, etc.).

    Heartbeating is critical for long-running activities:
    1. Proves to Temporal the activity is still making progress.
    2. If the worker crashes and stops heartbeating, Temporal reschedules the
       activity on another worker after `heartbeat_timeout` elapses.
    3. The heartbeat payload (optional) lets the new worker resume mid-task
       instead of restarting from scratch.

    Without heartbeating, a crashed worker causes the activity to hang until
    the `start_to_close_timeout` expires — potentially hours later.
    """
    logger.info("long_running_task starting, %d steps", steps)

    for i in range(steps):
        await asyncio.sleep(1)  # simulate a unit of work
        # Send heartbeat with progress so a replacement worker can resume here
        activity.heartbeat({"step": i + 1, "total": steps})
        logger.info("Heartbeat: step %d/%d", i + 1, steps)

    return f"Completed all {steps} steps"
