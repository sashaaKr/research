"""
Starts a SupportTicketWorkflow and optionally resolves it via signal.

Usage:
    python starter.py                    # let it escalate automatically
    python starter.py TKT-002 --resolve  # resolve after 5 seconds
"""
import asyncio
import sys

from temporalio.client import Client

from workflows import SupportTicketWorkflow

TASK_QUEUE = "support-queue"


async def main(ticket_id: str, auto_resolve: bool) -> None:
    client = await Client.connect("localhost:7233")

    handle = await client.start_workflow(
        SupportTicketWorkflow.run,
        args=[ticket_id, "Customer cannot log in after password reset"],
        id=f"ticket-{ticket_id}",
        task_queue=TASK_QUEUE,
    )
    print(f"Ticket workflow started: {handle.id!r}")
    print("Watch the worker terminal for timer events and escalations.")
    print()

    if auto_resolve:
        # Wait a few seconds, then send the resolve signal to demonstrate
        # that the timer branch is interrupted cleanly.
        await asyncio.sleep(5)
        await handle.signal(SupportTicketWorkflow.resolve, "Reset link re-sent, issue fixed")
        print("Resolution signal sent — workflow will complete now.")

    result = await handle.result()
    print(f"\nFinal result: {result!r}")


if __name__ == "__main__":
    ticket_id = sys.argv[1] if len(sys.argv) > 1 else "TKT-001"
    auto_resolve = "--resolve" in sys.argv
    asyncio.run(main(ticket_id, auto_resolve))
