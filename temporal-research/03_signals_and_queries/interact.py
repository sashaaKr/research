"""
Send signals to and run queries on a running OrderApprovalWorkflow.

Usage:
    python interact.py <workflow_id> status
    python interact.py <workflow_id> approve
    python interact.py <workflow_id> reject "Too expensive"

Example:
    python interact.py order-ORD-001 status
    python interact.py order-ORD-001 approve
"""
import asyncio
import sys

from temporalio.client import Client

from workflows import OrderApprovalWorkflow


async def main(workflow_id: str, action: str, *args: str) -> None:
    client = await Client.connect("localhost:7233")

    # get_workflow_handle() gives you a handle to an *existing* workflow
    # execution by ID. No network call happens yet.
    handle = client.get_workflow_handle(workflow_id)

    if action == "status":
        # Queries are synchronous — the server returns the current state immediately.
        status = await handle.query(OrderApprovalWorkflow.get_status)
        details = await handle.query(OrderApprovalWorkflow.get_details)
        print(f"Status : {status}")
        print(f"Details: {details}")

    elif action == "approve":
        # Signals are asynchronous — they're delivered to the workflow on its
        # next task queue poll. The call returns once the server acknowledges it.
        await handle.signal(OrderApprovalWorkflow.approve)
        print("Approval signal sent")

    elif action == "reject":
        reason = args[0] if args else "No reason given"
        await handle.signal(OrderApprovalWorkflow.reject, reason)
        print(f"Rejection signal sent: {reason!r}")

    else:
        print(f"Unknown action: {action!r}")
        print("Valid actions: status | approve | reject <reason>")
        sys.exit(1)


if __name__ == "__main__":
    if len(sys.argv) < 3:
        print(__doc__)
        sys.exit(1)

    asyncio.run(main(sys.argv[1], sys.argv[2], *sys.argv[3:]))
