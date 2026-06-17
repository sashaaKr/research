import asyncio
import logging
import random
from typing import List

from temporalio import activity

logger = logging.getLogger(__name__)


@activity.defn
async def process_item(item_id: str) -> str:
    """
    Simulate processing a single item (e.g., resizing an image, sending an email).
    Variable duration to show that children truly run in parallel.
    """
    delay = random.uniform(0.2, 1.0)
    logger.info("Processing item %s (will take %.1fs)", item_id, delay)
    await asyncio.sleep(delay)
    return f"processed:{item_id}"


@activity.defn
async def aggregate_results(batch_id: str, results: List[dict]) -> str:
    """Combine all child workflow results into a summary."""
    logger.info("Aggregating %d results for batch %s", len(results), batch_id)
    success_count = sum(1 for r in results if r.get("result", "").startswith("processed:"))
    return f"Batch {batch_id}: {success_count}/{len(results)} items completed"
