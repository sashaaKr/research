import asyncio
from datetime import timedelta
from typing import Optional

from temporalio import workflow

with workflow.unsafe.imports_allowed():
    from activities import cancel_order, process_order


@workflow.defn
class OrderApprovalWorkflow:
    """
    An order approval workflow that waits for a human decision.

    Demonstrates the two main ways external code interacts with a running workflow:

    SIGNALS  — push an event INTO the workflow (one-way, async).
               Use for: approvals, cancellations, progress updates, external events.

    QUERIES  — read state FROM the workflow (synchronous, no side-effects).
               Use for: dashboards, status checks, audit trails.
               Queries must be pure reads — never modify workflow state in a query.

    WAIT_CONDITION — suspends execution until a predicate becomes true
                     (or a timeout fires). This is how workflows wait for signals
                     without busy-polling.
    """

    def __init__(self) -> None:
        self._status = "pending_approval"
        self._approved: Optional[bool] = None
        self._rejection_reason: Optional[str] = None

    # ── Signals ───────────────────────────────────────────────────────────────

    @workflow.signal
    async def approve(self) -> None:
        """Signal: a manager approved the order."""
        workflow.logger.info("Received 'approve' signal")
        self._approved = True
        self._status = "approved"

    @workflow.signal
    async def reject(self, reason: str) -> None:
        """Signal: a manager rejected the order."""
        workflow.logger.info("Received 'reject' signal: %r", reason)
        self._approved = False
        self._rejection_reason = reason
        self._status = "rejected"

    # ── Queries ───────────────────────────────────────────────────────────────

    @workflow.query
    def get_status(self) -> str:
        """Returns the current status string."""
        return self._status

    @workflow.query
    def get_details(self) -> dict:
        """Returns full decision details."""
        return {
            "status": self._status,
            "approved": self._approved,
            "rejection_reason": self._rejection_reason,
        }

    # ── Entry point ───────────────────────────────────────────────────────────

    @workflow.run
    async def run(self, order_id: str, amount: float) -> str:
        workflow.logger.info("Order %s ($%.2f) awaiting approval", order_id, amount)

        # Suspend until a signal sets self._approved, or 24 hours pass.
        # workflow.wait_condition() is a durable timer — the worker can
        # restart and the workflow resumes exactly here.
        try:
            await workflow.wait_condition(
                lambda: self._approved is not None,
                timeout=timedelta(hours=24),
            )
        except asyncio.TimeoutError:
            self._status = "timed_out"
            return f"Order {order_id} expired — no decision within 24 hours"

        if self._approved:
            return await workflow.execute_activity(
                process_order,
                order_id,
                start_to_close_timeout=timedelta(seconds=30),
            )
        else:
            return await workflow.execute_activity(
                cancel_order,
                order_id,
                self._rejection_reason or "No reason provided",
                start_to_close_timeout=timedelta(seconds=30),
            )
