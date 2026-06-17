"""
Run this in a terminal before the starter.
Keep it running — it polls for work continuously.
"""
import asyncio
import logging

from temporalio.client import Client
from temporalio.worker import Worker

from activities import say_hello
from workflows import HelloWorldWorkflow

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")

TASK_QUEUE = "hello-world-queue"


async def main() -> None:
    # Connect to the Temporal server (default: localhost:7233)
    client = await Client.connect("localhost:7233")

    # A Worker:
    # - Registers the workflows and activities it can execute.
    # - Long-polls the task queue for tasks, runs them locally.
    # - Multiple workers can share the same task queue for horizontal scaling.
    worker = Worker(
        client,
        task_queue=TASK_QUEUE,
        workflows=[HelloWorldWorkflow],
        activities=[say_hello],
    )

    print(f"Worker started. Polling task queue: {TASK_QUEUE!r}")
    print("Press Ctrl+C to stop.")
    await worker.run()


if __name__ == "__main__":
    asyncio.run(main())
