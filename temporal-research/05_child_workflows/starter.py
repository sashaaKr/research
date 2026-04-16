"""
Starts a BatchWorkflow with a list of items and prints the aggregated result.

Usage:
    python starter.py                           # 6 default items
    python starter.py BATCH-002 item-a item-b   # custom batch
"""
import asyncio
import sys

from temporalio.client import Client

from workflows import BatchWorkflow

TASK_QUEUE = "batch-queue"


async def main(batch_id: str, items: list) -> None:
    client = await Client.connect("localhost:7233")

    print(f"Starting batch {batch_id!r} with {len(items)} items: {items}")
    print("Watch the Web UI — each item will appear as its own child workflow.")
    print()

    result = await client.execute_workflow(
        BatchWorkflow.run,
        args=[batch_id, items],
        id=f"batch-{batch_id}",
        task_queue=TASK_QUEUE,
    )

    print(f"Batch complete!")
    print(f"  Summary  : {result['summary']}")
    print(f"  Processed: {result['items_processed']}")
    print(f"  Results  : {result['results']}")


if __name__ == "__main__":
    batch_id = sys.argv[1] if len(sys.argv) > 1 else "BATCH-001"
    items = sys.argv[2:] if len(sys.argv) > 2 else ["img-1", "img-2", "img-3", "img-4", "img-5", "img-6"]
    asyncio.run(main(batch_id, items))
