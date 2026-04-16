import logging

from temporalio import activity

logger = logging.getLogger(__name__)


@activity.defn
async def process_order(order_id: str) -> str:
    """Fulfil an approved order (charge card, trigger shipment, etc.)."""
    logger.info("Processing order %s", order_id)
    return f"Order {order_id} processed and shipped"


@activity.defn
async def cancel_order(order_id: str, reason: str) -> str:
    """Cancel a rejected order and release any holds."""
    logger.info("Cancelling order %s: %s", order_id, reason)
    return f"Order {order_id} cancelled — reason: {reason}"
