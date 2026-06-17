"""
Temporal worker entry point — configured entirely via environment variables.

  WORKER_TYPE=orchestrator  registers workflows only  (lightweight pods)
  WORKER_TYPE=activities    registers activities only  (resource-intensive pods)
  WORKER_TYPE=all           registers both             (default — good for local dev)

Environment variables
─────────────────────
TEMPORAL_HOST                   gRPC address          (default: localhost:7233)
TEMPORAL_NAMESPACE              Temporal namespace     (default: default)
TEMPORAL_TLS_CERT               PEM cert — set for Temporal Cloud
TEMPORAL_TLS_KEY                PEM key  — set for Temporal Cloud

WORKER_TYPE                     orchestrator | activities | all
TASK_QUEUE                      Task queue this worker polls
ACTIVITY_TASK_QUEUE             Queue the orchestrator routes activities to.
                                Defaults to TASK_QUEUE (same queue, useful for dev).
                                Set to "provisioning-queue" in the orchestrator
                                Deployment so activity tasks land on the activities pods.

MAX_CONCURRENT_ACTIVITIES       Max parallel activities per pod  (default: 50)
MAX_CONCURRENT_WORKFLOW_TASKS   Max parallel workflow tasks per pod (default: 100)
GRACEFUL_SHUTDOWN_SECS          Drain window on SIGTERM           (default: 30)
                                Must be < K8s terminationGracePeriodSeconds.
"""
import asyncio
import logging
import os
from datetime import timedelta

from temporalio.client import Client, TLSConfig
from temporalio.worker import Worker

from activities import (
    provision_cache,
    provision_database,
    provision_load_balancer,
    provision_server,
    provision_subnet,
    provision_vpc,
    register_dns,
    run_health_check,
)
from interpreter import TemplateRunnerWorkflow

# ── Config ────────────────────────────────────────────────────────────────────

TEMPORAL_HOST      = os.getenv("TEMPORAL_HOST", "localhost:7233")
TEMPORAL_NAMESPACE = os.getenv("TEMPORAL_NAMESPACE", "default")
TEMPORAL_TLS_CERT  = os.getenv("TEMPORAL_TLS_CERT", "")
TEMPORAL_TLS_KEY   = os.getenv("TEMPORAL_TLS_KEY", "")

WORKER_TYPE         = os.getenv("WORKER_TYPE", "all")
TASK_QUEUE          = os.getenv("TASK_QUEUE", "template-runner-queue")
ACTIVITY_TASK_QUEUE = os.getenv("ACTIVITY_TASK_QUEUE", TASK_QUEUE)

MAX_CONCURRENT_ACTIVITIES     = int(os.getenv("MAX_CONCURRENT_ACTIVITIES", "50"))
MAX_CONCURRENT_WORKFLOW_TASKS = int(os.getenv("MAX_CONCURRENT_WORKFLOW_TASKS", "100"))
GRACEFUL_SHUTDOWN_SECS        = int(os.getenv("GRACEFUL_SHUTDOWN_SECS", "30"))

# ── Logging ───────────────────────────────────────────────────────────────────

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s %(levelname)s %(name)s %(message)s",
)
logger = logging.getLogger(__name__)

# ── Registration lists ────────────────────────────────────────────────────────

_WORKFLOWS = [TemplateRunnerWorkflow]

_ACTIVITIES = [
    provision_vpc,
    provision_subnet,
    provision_server,
    provision_database,
    provision_cache,
    provision_load_balancer,
    register_dns,
    run_health_check,
]


# ── Entry point ───────────────────────────────────────────────────────────────

async def main() -> None:
    # TLS for Temporal Cloud; plain TCP for self-hosted
    tls: bool | TLSConfig = False
    if TEMPORAL_TLS_CERT and TEMPORAL_TLS_KEY:
        tls = TLSConfig(
            client_cert=TEMPORAL_TLS_CERT.encode(),
            client_private_key=TEMPORAL_TLS_KEY.encode(),
        )
        logger.info("TLS enabled — connecting to Temporal Cloud")

    client = await Client.connect(
        TEMPORAL_HOST,
        namespace=TEMPORAL_NAMESPACE,
        tls=tls,
    )

    worker = Worker(
        client,
        task_queue=TASK_QUEUE,
        workflows  = _WORKFLOWS  if WORKER_TYPE in ("orchestrator", "all") else [],
        activities = _ACTIVITIES if WORKER_TYPE in ("activities",   "all") else [],
        max_concurrent_activities     = MAX_CONCURRENT_ACTIVITIES,
        max_concurrent_workflow_tasks = MAX_CONCURRENT_WORKFLOW_TASKS,
        # On SIGTERM Temporal stops polling and waits for in-flight tasks to finish.
        # K8s sends SIGTERM then waits terminationGracePeriodSeconds before SIGKILL.
        # Keep this value a few seconds below terminationGracePeriodSeconds.
        graceful_shutdown_timeout = timedelta(seconds=GRACEFUL_SHUTDOWN_SECS),
    )

    logger.info(
        "Worker ready | type=%s  queue=%s  activity_queue=%s  "
        "max_activities=%d  max_wf_tasks=%d",
        WORKER_TYPE, TASK_QUEUE, ACTIVITY_TASK_QUEUE,
        MAX_CONCURRENT_ACTIVITIES, MAX_CONCURRENT_WORKFLOW_TASKS,
    )

    await worker.run()


if __name__ == "__main__":
    asyncio.run(main())
