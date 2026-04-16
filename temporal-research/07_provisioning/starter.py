"""
Provisions an environment and streams live progress via queries.

Scenarios:
  dev     — 1 server, no DB, no cache, no LB  (fast, minimal)
  staging — 2 servers + DB                    (LB included, no cache)
  prod    — 4 servers + DB + cache            (full stack)

Usage:
    python starter.py [dev|staging|prod] [--cancel]

    --cancel  sends a cancellation signal after the network phase
"""
import asyncio
import dataclasses
import json
import sys

from temporalio.client import Client
from temporalio.exceptions import WorkflowFailureError

from models import ProvisioningRequest
from workflows import EnvironmentProvisioningWorkflow

TASK_QUEUE = "provisioning-queue"

SCENARIOS: dict[str, ProvisioningRequest] = {
    "dev": ProvisioningRequest(
        environment="dev",
        region="us-east-1",
        server_count=1,
    ),
    "staging": ProvisioningRequest(
        environment="staging",
        region="us-east-1",
        server_count=2,
        include_database=True,
    ),
    "prod": ProvisioningRequest(
        environment="prod",
        region="us-east-1",
        server_count=4,
        instance_type="t3.large",
        include_database=True,
        db_instance_type="db.t3.medium",
        include_cache=True,
    ),
}


async def stream_progress(handle, poll_interval: float = 0.5) -> None:
    """Query the workflow every 500ms and print live phase updates."""
    last_phase = None
    while True:
        try:
            progress = await handle.query(EnvironmentProvisioningWorkflow.get_progress)
        except Exception:
            return

        phase = progress.get("phase", "?")
        if phase != last_phase:
            print(f"\n  → phase: {phase}", end="", flush=True)
            last_phase = phase

        details = {k: v for k, v in progress.items() if k != "phase"}
        if details:
            detail_str = "  " + "  ".join(f"{k}={v}" for k, v in details.items())
            print(f"\r  → phase: {phase:<16}{detail_str}", end="", flush=True)

        if phase == "complete":
            print()
            return

        await asyncio.sleep(poll_interval)


async def main(scenario: str, send_cancel: bool) -> None:
    req = SCENARIOS[scenario]
    client = await Client.connect("localhost:7233")

    print(f"Provisioning environment: {scenario!r}")
    print(f"  servers={req.server_count}  db={req.include_database}  "
          f"cache={req.include_cache}  region={req.region}")
    print()

    handle = await client.start_workflow(
        EnvironmentProvisioningWorkflow.run,
        req,
        id=f"provision-{req.environment}",
        task_queue=TASK_QUEUE,
    )

    if send_cancel:
        # Wait for the network phase to finish, then cancel
        await asyncio.sleep(2)
        await handle.signal(EnvironmentProvisioningWorkflow.cancel_provisioning)
        print("  [cancellation signal sent]")

    # Stream progress while waiting for the result
    await stream_progress(handle)

    try:
        result = await handle.result()
    except WorkflowFailureError as e:
        print(f"\nProvisioning failed: {e.__cause__}")
        return

    print(f"\nEnvironment {result.environment!r} is {result.status.upper()}")
    print(f"  VPC            : {result.vpc.vpc_id}")
    print(f"  Subnets ({len(result.subnets)})     : {[s.subnet_id for s in result.subnets]}")
    print(f"  Servers ({len(result.servers)})     : {[s.server_id for s in result.servers]}")
    if result.load_balancer:
        print(f"  Load Balancer  : {result.load_balancer.dns_name}")
    if result.database:
        print(f"  Database       : {result.database.endpoint}")
    if result.cache:
        print(f"  Cache          : {result.cache.endpoint}")
    print(f"  DNS records ({len(result.dns_records)})  : {[r.name for r in result.dns_records]}")


if __name__ == "__main__":
    scenario = next((a for a in sys.argv[1:] if a in SCENARIOS), "dev")
    send_cancel = "--cancel" in sys.argv
    asyncio.run(main(scenario, send_cancel))
