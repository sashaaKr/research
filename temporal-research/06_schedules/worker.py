import asyncio
import logging

from temporalio.client import Client
from temporalio.worker import Worker

from activities import collect_metrics, publish_report
from workflows import MetricsReportWorkflow

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")

TASK_QUEUE = "schedules-queue"


async def main() -> None:
    client = await Client.connect("localhost:7233")
    worker = Worker(
        client,
        task_queue=TASK_QUEUE,
        workflows=[MetricsReportWorkflow],
        activities=[collect_metrics, publish_report],
    )
    print(f"Worker started. Polling task queue: {TASK_QUEUE!r}")
    await worker.run()


if __name__ == "__main__":
    asyncio.run(main())
