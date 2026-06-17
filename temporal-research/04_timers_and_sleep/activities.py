import logging

from temporalio import activity

logger = logging.getLogger(__name__)


@activity.defn
async def send_reminder(ticket_id: str) -> None:
    """Notify the on-call agent that a ticket has been waiting too long."""
    logger.info("Sending reminder for ticket %s", ticket_id)
    # In production: send Slack message, PagerDuty alert, email, etc.
    print(f"  [REMINDER] Ticket {ticket_id} is waiting for a response!")


@activity.defn
async def escalate_to_manager(ticket_id: str) -> None:
    """Page the manager when a ticket has gone unanswered for too long."""
    logger.info("Escalating ticket %s to manager", ticket_id)
    print(f"  [ESCALATION] Ticket {ticket_id} escalated to on-call manager!")


@activity.defn
async def close_ticket(ticket_id: str, resolution: str) -> str:
    """Record the resolution and close the ticket."""
    logger.info("Closing ticket %s: %s", ticket_id, resolution)
    return f"Ticket {ticket_id} closed — {resolution}"
