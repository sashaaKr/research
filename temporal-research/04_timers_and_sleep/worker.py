import asyncio
import logging

from temporalio.client import Client
from temporalio.worker import Worker

from activities import close_ticket, escalate_to_manager, send_reminder
from workflows import SupportTicketWorkflow

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")

TASK_QUEUE = "support-queue"


async def main() -> None:
    client = await Client.connect("localhost:7233")
    worker = Worker(
        client,
        task_queue=TASK_QUEUE,
        workflows=[SupportTicketWorkflow],
        activities=[send_reminder, escalate_to_manager, close_ticket],
    )
    print(f"Worker started. Polling task queue: {TASK_QUEUE!r}")
    await worker.run()


if __name__ == "__main__":
    asyncio.run(main())
