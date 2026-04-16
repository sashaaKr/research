from datetime import timedelta

from temporalio import workflow
from temporalio.common import RetryPolicy

with workflow.unsafe.imports_allowed():
    from activities import flaky_service_call, validate_business_rule, long_running_task


@workflow.defn
class RetryDemoWorkflow:
    """
    Three scenarios showing Temporal's retry and error handling system.

    Pass scenario="flaky"        → custom retry policy with exponential backoff
    Pass scenario="non_retryable" → non-retryable error fails the workflow fast
    Pass scenario="heartbeat"    → long-running activity with heartbeating
    """

    @workflow.run
    async def run(self, scenario: str) -> str:
        if scenario == "flaky":
            return await self._demo_retry()
        elif scenario == "non_retryable":
            return await self._demo_non_retryable()
        elif scenario == "heartbeat":
            return await self._demo_heartbeat()
        else:
            raise ValueError(f"Unknown scenario: {scenario!r}")

    async def _demo_retry(self) -> str:
        """
        Custom RetryPolicy:
        - Start with 1s delay, double each time (backoff_coefficient=2.0).
        - Cap at 10s per attempt.
        - Give up after 5 total attempts.

        The activity fails 3 times then succeeds → observe attempt counts in logs.
        """
        return await workflow.execute_activity(
            flaky_service_call,
            3,  # fail the first 3 attempts
            start_to_close_timeout=timedelta(seconds=60),
            retry_policy=RetryPolicy(
                initial_interval=timedelta(seconds=1),
                backoff_coefficient=2.0,   # delays: 1s, 2s, 4s, 8s ...
                maximum_interval=timedelta(seconds=10),
                maximum_attempts=5,
                # non_retryable_error_types=["MyFatalError"]  # can filter by type name
            ),
        )

    async def _demo_non_retryable(self) -> str:
        """
        The activity raises ApplicationError(non_retryable=True).
        Temporal propagates it immediately without any retry.
        The workflow itself will fail.
        """
        return await workflow.execute_activity(
            validate_business_rule,
            -42,  # invalid → triggers non-retryable error
            start_to_close_timeout=timedelta(seconds=10),
        )

    async def _demo_heartbeat(self) -> str:
        """
        Long-running activity with a heartbeat_timeout.

        If the activity stops sending heartbeats for longer than
        heartbeat_timeout, Temporal marks it as failed and retries it.
        The new attempt can read the last heartbeat payload to resume
        from a checkpoint rather than the beginning.
        """
        return await workflow.execute_activity(
            long_running_task,
            5,  # 5 one-second steps
            start_to_close_timeout=timedelta(seconds=120),
            heartbeat_timeout=timedelta(seconds=10),  # must heartbeat every 10s
        )
