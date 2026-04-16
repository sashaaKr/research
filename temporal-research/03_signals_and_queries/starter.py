"""
Starts an OrderApprovalWorkflow and prints instructions for interacting with it.

Usage:
    python starter.py [order_id] [amount]
    python starter.py ORD-007 2500
"""
import asyncio
import sys

from temporalio.client import Client

from workflows import OrderApprovalWorkflow

TASK_QUEUE = "approval-queue"


async def main(order_id: str, amount: float) -> None:
    client = await Client.connect("localhost:7233")

    # start_workflow() submits the workflow but does NOT wait for its result.
    # This is useful when you need the workflow ID before it finishes.
    handle = await client.start_workflow(
        OrderApprovalWorkflow.run,
        args=[order_id, amount],
        id=f"order-{order_id}",
        task_queue=TASK_QUEUE,
    )

    print(f"Workflow started: {handle.id!r}")
    print()
    print("The workflow is now paused, waiting for a signal.")
    print("In another terminal, run one of:")
    print(f"  python interact.py {handle.id} status")
    print(f"  python interact.py {handle.id} approve")
    print(f'  python interact.py {handle.id} reject "Too expensive"')
    print()

    # Wait for the workflow to finish (will block until signal arrives)
    result = await handle.result()
    print(f"Final result: {result!r}")


if __name__ == "__main__":
    order_id = sys.argv[1] if len(sys.argv) > 1 else "ORD-001"
    amount = float(sys.argv[2]) if len(sys.argv) > 2 else 1500.00
    asyncio.run(main(order_id, amount))
