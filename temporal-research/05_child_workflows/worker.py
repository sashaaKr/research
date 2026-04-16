import asyncio
import logging

from temporalio.client import Client
from temporalio.worker import Worker

from activities import aggregate_results, process_item
from workflows import BatchWorkflow, ItemWorkflow

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")

TASK_QUEUE = "batch-queue"


async def main() -> None:
    client = await Client.connect("localhost:7233")

    # Both parent and child workflows must be registered on the worker
    # that polls the task queue they run on. Here both use the same queue,
    # but in production you might route children to a dedicated worker pool.
    worker = Worker(
        client,
        task_queue=TASK_QUEUE,
        workflows=[BatchWorkflow, ItemWorkflow],
        activities=[process_item, aggregate_results],
    )
    print(f"Worker started. Polling task queue: {TASK_QUEUE!r}")
    await worker.run()


if __name__ == "__main__":
    asyncio.run(main())
