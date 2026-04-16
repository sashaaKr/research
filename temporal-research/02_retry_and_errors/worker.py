import asyncio
import logging

from temporalio.client import Client
from temporalio.worker import Worker

from activities import flaky_service_call, long_running_task, validate_business_rule
from workflows import RetryDemoWorkflow

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")

TASK_QUEUE = "retry-demo-queue"


async def main() -> None:
    client = await Client.connect("localhost:7233")
    worker = Worker(
        client,
        task_queue=TASK_QUEUE,
        workflows=[RetryDemoWorkflow],
        activities=[flaky_service_call, validate_business_rule, long_running_task],
    )
    print(f"Worker started. Polling task queue: {TASK_QUEUE!r}")
    await worker.run()


if __name__ == "__main__":
    asyncio.run(main())
