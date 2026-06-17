import asyncio
import logging

from temporalio.client import Client
from temporalio.worker import Worker

from activities import (
    provision_cache,
    provision_database,
    provision_load_balancer,
    provision_server,
    provision_subnet,
    provision_vpc,
    register_dns_records,
    run_health_check,
)
from workflows import EnvironmentProvisioningWorkflow, ServerProvisioningWorkflow

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")

TASK_QUEUE = "provisioning-queue"


async def main() -> None:
    client = await Client.connect("localhost:7233")
    worker = Worker(
        client,
        task_queue=TASK_QUEUE,
        workflows=[EnvironmentProvisioningWorkflow, ServerProvisioningWorkflow],
        activities=[
            provision_vpc,
            provision_subnet,
            provision_server,
            provision_database,
            provision_cache,
            provision_load_balancer,
            register_dns_records,
            run_health_check,
        ],
    )
    print(f"Worker started. Polling task queue: {TASK_QUEUE!r}")
    await worker.run()


if __name__ == "__main__":
    asyncio.run(main())
