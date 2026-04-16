import asyncio
from datetime import timedelta
from typing import List

from temporalio import workflow

with workflow.unsafe.imports_allowed():
    from activities import aggregate_results, process_item


@workflow.defn
class ItemWorkflow:
    """
    Child workflow: processes a single item.

    Each child is a full workflow execution — it has its own workflow ID,
    event history, retry logic, and is independently visible in the Web UI.
    This is different from just calling an activity: children can be
    long-running, have their own signals/queries, and run on different workers.
    """

    @workflow.run
    async def run(self, item_id: str) -> dict:
        result = await workflow.execute_activity(
            process_item,
            item_id,
            start_to_close_timeout=timedelta(seconds=30),
        )
        return {"item_id": item_id, "result": result}


@workflow.defn
class BatchWorkflow:
    """
    Parent workflow: fans out to N child workflows, waits for all, aggregates.

    Key concepts:
    - workflow.execute_child_workflow() starts a child and returns its result.
    - asyncio.gather() runs all children concurrently within the workflow.
      (Each awaitable is a Temporal workflow task, not a thread or OS process.)
    - Children have their own unique IDs, visible and searchable in the UI.
    - If the parent is cancelled, all pending children are cancelled too.
    - For fire-and-forget children, use workflow.start_child_workflow() without
      awaiting the handle — the child runs independently of the parent.

    Fan-out is the primary reason to use child workflows. If each item's
    processing is trivial, prefer executing activities directly (less overhead).
    Use child workflows when each unit of work is complex, long-running, or
    needs its own durability guarantees.
    """

    @workflow.run
    async def run(self, batch_id: str, items: List[str]) -> dict:
        workflow.logger.info(
            "Batch %s: launching %d child workflows", batch_id, len(items)
        )

        # Build all child coroutines — they don't start until gathered
        child_coros = [
            workflow.execute_child_workflow(
                ItemWorkflow.run,
                item_id,
                id=f"{batch_id}-item-{item_id}",  # unique, human-readable ID
            )
            for item_id in items
        ]

        # Run all children concurrently; wait for all to complete.
        # Temporal runs these as separate workflow executions in parallel.
        results: List[dict] = await asyncio.gather(*child_coros)

        summary = await workflow.execute_activity(
            aggregate_results,
            batch_id,
            results,
            start_to_close_timeout=timedelta(seconds=30),
        )

        return {
            "batch_id": batch_id,
            "items_processed": len(items),
            "summary": summary,
            "results": results,
        }
