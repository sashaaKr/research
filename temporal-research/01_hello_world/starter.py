"""
Submits a HelloWorldWorkflow execution and prints the result.

The client does NOT run workflow or activity code — it just sends a
request to the Temporal server. The actual execution happens in the worker.
"""
import asyncio

from temporalio.client import Client

from workflows import HelloWorldWorkflow

TASK_QUEUE = "hello-world-queue"


async def main() -> None:
    client = await Client.connect("localhost:7233")

    # execute_workflow() = start + wait for result in one call.
    # The workflow `id` is user-defined and must be unique per namespace.
    # Re-submitting with the same ID returns the existing execution (idempotent).
    result = await client.execute_workflow(
        HelloWorldWorkflow.run,
        "World",
        id="hello-world-01",
        task_queue=TASK_QUEUE,
    )

    print(f"Workflow result: {result!r}")
    print()
    print("Inspect the execution in the Web UI:")
    print("  temporal server start-dev → http://localhost:8233")
    print("  docker compose          → http://localhost:8080")


if __name__ == "__main__":
    asyncio.run(main())
