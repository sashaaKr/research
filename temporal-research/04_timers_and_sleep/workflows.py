import asyncio
from datetime import timedelta
from typing import Optional

from temporalio import workflow

with workflow.unsafe.imports_passed_through():
    from activities import close_ticket, escalate_to_manager, send_reminder


@workflow.defn
class SupportTicketWorkflow:
    """
    A support escalation workflow that demonstrates durable timers.

    Timeline (demo uses short durations; swap for real ones):
        T+0s   → Ticket opened, waiting for agent response
        T+20s  → No response → send reminder
        T+40s  → Still no response → escalate to manager
        T+any  → Agent sends `resolve` signal → ticket closed

    WHY durable timers matter:
        Regular `asyncio.sleep(hours=1)` would be lost if the worker process
        restarts. Temporal persists the timer in the server — the workflow
        resumes at exactly the right time even after a full server restart,
        worker crash, or deployment. A timer set for 30 days will fire in 30
        days, guaranteed.

    RULE: Never use asyncio.sleep() inside a workflow for business logic.
          Use workflow.wait_condition(timeout=...) or let the timeout on
          wait_condition drive your time-based state machine.
    """

    def __init__(self) -> None:
        self._resolved = False
        self._resolution: Optional[str] = None

    @workflow.signal
    async def resolve(self, resolution: str) -> None:
        """Signal from an agent or manager: ticket has been resolved."""
        workflow.logger.info("Received resolve signal: %r", resolution)
        self._resolved = True
        self._resolution = resolution

    @workflow.query
    def status(self) -> dict:
        return {"resolved": self._resolved, "resolution": self._resolution}

    @workflow.run
    async def run(self, ticket_id: str, description: str) -> str:
        workflow.logger.info("Ticket %s opened: %r", ticket_id, description)

        # Phase 1: Wait up to 20s for an initial response (use hours=1 in prod)
        try:
            await workflow.wait_condition(
                lambda: self._resolved,
                timeout=timedelta(seconds=20),
            )
        except asyncio.TimeoutError:
            pass  # No response — proceed to reminder

        if self._resolved:
            return await self._close(ticket_id)

        # Phase 2: Send a reminder, wait another 20s
        await workflow.execute_activity(
            send_reminder, ticket_id, start_to_close_timeout=timedelta(seconds=30)
        )
        try:
            await workflow.wait_condition(
                lambda: self._resolved,
                timeout=timedelta(seconds=20),
            )
        except asyncio.TimeoutError:
            pass

        if self._resolved:
            return await self._close(ticket_id)

        # Phase 3: Escalate — wait indefinitely (manager will resolve it)
        await workflow.execute_activity(
            escalate_to_manager,
            ticket_id,
            start_to_close_timeout=timedelta(seconds=30),
        )
        await workflow.wait_condition(lambda: self._resolved)

        return await self._close(ticket_id)

    async def _close(self, ticket_id: str) -> str:
        return await workflow.execute_activity(
            close_ticket,
            ticket_id,
            self._resolution or "Resolved",
            start_to_close_timeout=timedelta(seconds=30),
        )
