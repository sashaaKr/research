import logging

from temporalio import activity

logger = logging.getLogger(__name__)


@activity.defn
async def say_hello(name: str) -> str:
    """
    A simple activity that greets someone.

    Activities are the units of work in Temporal — they do the actual I/O
    (HTTP calls, DB queries, file operations, etc.). They run in a worker
    process *outside* the workflow sandbox, so any Python library is safe here.

    Key guarantees:
    - Temporal retries failed activities automatically (configurable).
    - activity.info() gives you metadata: attempt number, workflow ID, etc.
    """
    logger.info("say_hello called for %r (attempt %d)", name, activity.info().attempt)
    return f"Hello, {name}!"
