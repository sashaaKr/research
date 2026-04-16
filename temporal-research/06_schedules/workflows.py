from datetime import timedelta

from temporalio import workflow

with workflow.unsafe.imports_allowed():
    from activities import collect_metrics, publish_report


@workflow.defn
class MetricsReportWorkflow:
    """
    A short-lived workflow triggered by a Schedule.

    Each schedule tick creates a brand-new workflow execution. The workflow
    itself is stateless — all state management (overlap policy, history,
    pause/unpause) is handled by the Schedule object on the server.

    The `workflow.info().scheduled_start_time` field tells you when the
    schedule intended to run this execution (useful for backfill scenarios).
    """

    @workflow.run
    async def run(self, environment: str) -> str:
        scheduled_at = workflow.info().scheduled_start_time
        workflow.logger.info(
            "MetricsReport for %s (scheduled at %s)", environment, scheduled_at
        )

        metrics = await workflow.execute_activity(
            collect_metrics,
            environment,
            start_to_close_timeout=timedelta(seconds=30),
        )

        result = await workflow.execute_activity(
            publish_report,
            metrics,
            start_to_close_timeout=timedelta(seconds=30),
        )

        return result
