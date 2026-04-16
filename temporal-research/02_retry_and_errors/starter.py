"""
Usage:
    python starter.py flaky          # retries 3x then succeeds
    python starter.py non_retryable  # fails immediately (no retries)
    python starter.py heartbeat      # 5-step task with heartbeats
"""
import asyncio
import sys

from temporalio.client import Client
from temporalio.exceptions import WorkflowFailureError

from workflows import RetryDemoWorkflow

TASK_QUEUE = "retry-demo-queue"


async def main(scenario: str) -> None:
    client = await Client.connect("localhost:7233")

    print(f"Running scenario: {scenario!r}")
    try:
        result = await client.execute_workflow(
            RetryDemoWorkflow.run,
            scenario,
            id=f"retry-demo-{scenario}",
            task_queue=TASK_QUEUE,
        )
        print(f"Result: {result!r}")
    except WorkflowFailureError as e:
        # WorkflowFailureError wraps the root cause. Unwrap it for a clear message.
        print(f"Workflow failed: {e.__cause__}")


if __name__ == "__main__":
    scenario = sys.argv[1] if len(sys.argv) > 1 else "flaky"
    asyncio.run(main(scenario))
