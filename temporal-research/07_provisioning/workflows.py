import asyncio
from datetime import timedelta
from typing import Optional

from temporalio import workflow
from temporalio.exceptions import ApplicationError

with workflow.unsafe.imports_passed_through():
    from activities import (
        provision_cache,
        provision_database,
        provision_load_balancer,
        provision_server,
        provision_subnet,
        provision_vpc,
        register_dns_records,
        run_health_check,
    )
    from models import (
        CacheResult,
        DatabaseResult,
        DnsRecord,
        EnvironmentManifest,
        LoadBalancerResult,
        ProvisioningRequest,
        ServerResult,
        SubnetResult,
        VpcResult,
    )

_ACT = {"start_to_close_timeout": timedelta(seconds=60)}


# ── Child workflow ─────────────────────────────────────────────────────────────

@workflow.defn
class ServerProvisioningWorkflow:
    """
    Provisions a single server. Used as a child workflow so each server
    gets its own workflow ID, event history, and retry scope.

    In a real system this workflow would be more complex: install software,
    register with a service mesh, run smoke tests, etc.
    """

    @workflow.run
    async def run(
        self,
        vpc_id: str,
        subnet_id: str,
        instance_type: str,
        az: str,
        environment: str,
    ) -> ServerResult:
        return await workflow.execute_activity(
            provision_server,
            args=[vpc_id, subnet_id, instance_type, az, environment],
            **_ACT,
        )


# ── Main workflow ──────────────────────────────────────────────────────────────

@workflow.defn
class EnvironmentProvisioningWorkflow:
    """
    Provisions a complete cloud environment in five phases.

    Phase 1 — Network (sequential then parallel)
      Provision VPC first, then all subnets concurrently across 3 AZs.

    Phase 2 — Compute + Data (fully parallel, some conditional)
      Fan out N server child-workflows concurrently.
      Simultaneously provision database (if requested) and cache (if requested).
      All three groups run in parallel — servers don't wait for DB and vice versa.

    Phase 3 — Load Balancer (conditional)
      Only provisioned when server_count > 1. Skipped for single-server envs.

    Phase 4 — DNS
      Build records for every endpoint, register them all concurrently.

    Phase 5 — Health Checks (parallel)
      Probe every provisioned resource simultaneously.

    Signals: cancel_provisioning — soft-cancel between phases
    Queries: get_progress        — live phase + resource inventory
    """

    def __init__(self) -> None:
        self._phase = "pending"
        self._progress: dict = {}
        self._cancel_requested = False

    @workflow.signal
    async def cancel_provisioning(self) -> None:
        workflow.logger.info("Cancellation requested")
        self._cancel_requested = True

    @workflow.query
    def get_progress(self) -> dict:
        return {"phase": self._phase, **self._progress}

    @workflow.run
    async def run(self, req: ProvisioningRequest) -> EnvironmentManifest:
        wf_id = workflow.info().workflow_id

        # ── Phase 1: Network ──────────────────────────────────────────────────
        self._phase = "network"

        vpc: VpcResult = await workflow.execute_activity(
            provision_vpc, req.region, **_ACT
        )
        self._progress["vpc_id"] = vpc.vpc_id

        # Subnets across 3 AZs — provision all three simultaneously
        azs = [f"{req.region}a", f"{req.region}b", f"{req.region}c"]
        subnets: list[SubnetResult] = await asyncio.gather(*[
            workflow.execute_activity(
                provision_subnet,
                args=[vpc.vpc_id, az, f"10.0.{i}.0/24"],
                **_ACT,
            )
            for i, az in enumerate(azs)
        ])
        self._progress["subnet_ids"] = [s.subnet_id for s in subnets]

        self._check_cancel("after network phase")

        # ── Phase 2: Compute + Database + Cache (all parallel) ────────────────
        self._phase = "compute"

        # N server child-workflows, round-robining across subnets/AZs
        server_coros = [
            workflow.execute_child_workflow(
                ServerProvisioningWorkflow.run,
                args=[
                    vpc.vpc_id,
                    subnets[i % len(subnets)].subnet_id,
                    req.instance_type,
                    azs[i % len(azs)],
                    req.environment,
                ],
                id=f"{wf_id}-server-{i}",
            )
            for i in range(req.server_count)
        ]

        # Conditional coroutines — resolve to None when not requested
        async def maybe_db() -> Optional[DatabaseResult]:
            if not req.include_database:
                return None
            return await workflow.execute_activity(
                provision_database,
                args=[vpc.vpc_id, [s.subnet_id for s in subnets],
                      req.db_instance_type, req.environment],
                **_ACT,
            )

        async def maybe_cache() -> Optional[CacheResult]:
            if not req.include_cache:
                return None
            return await workflow.execute_activity(
                provision_cache,
                args=[vpc.vpc_id, subnets[0].subnet_id, req.environment],
                **_ACT,
            )

        # asyncio.gather runs all concurrently:
        #   - inner gather fans out N server child-workflows
        #   - maybe_db() and maybe_cache() run in parallel with the servers
        servers, database, cache = await asyncio.gather(
            asyncio.gather(*server_coros),
            maybe_db(),
            maybe_cache(),
        )

        self._progress["server_ids"] = [s.server_id for s in servers]
        if database:
            self._progress["database"] = database.endpoint
        if cache:
            self._progress["cache"] = cache.endpoint

        self._check_cancel("after compute phase")

        # ── Phase 3: Load Balancer (conditional) ─────────────────────────────
        self._phase = "load_balancer"
        load_balancer: Optional[LoadBalancerResult] = None

        if req.server_count > 1:
            load_balancer = await workflow.execute_activity(
                provision_load_balancer,
                args=[
                    vpc.vpc_id,
                    [s.subnet_id for s in subnets],
                    [s.server_id for s in servers],
                    req.environment,
                ],
                **_ACT,
            )
            self._progress["load_balancer"] = load_balancer.dns_name
        else:
            workflow.logger.info("Skipping load balancer (single server)")

        # ── Phase 4: DNS ──────────────────────────────────────────────────────
        self._phase = "dns"

        dns_records = _build_dns_records(req.environment, servers, load_balancer, database, cache)
        registered = await workflow.execute_activity(
            register_dns_records, args=[dns_records, req.environment], **_ACT
        )
        self._progress["dns_records"] = len(registered)

        # ── Phase 5: Health Checks (all parallel) ────────────────────────────
        self._phase = "health_checks"

        check_targets = [(s.server_id, s.private_ip) for s in servers]
        if load_balancer:
            check_targets.append((load_balancer.lb_id, load_balancer.dns_name))
        if database:
            check_targets.append((database.db_id, database.endpoint))
        if cache:
            check_targets.append((cache.cache_id, cache.endpoint))

        health_results = await asyncio.gather(*[
            workflow.execute_activity(
                run_health_check, args=[rid, ep], **_ACT
            )
            for rid, ep in check_targets
        ])

        healthy = sum(1 for r in health_results if r["healthy"])
        self._progress["health"] = f"{healthy}/{len(health_results)} healthy"

        self._phase = "complete"

        return EnvironmentManifest(
            environment=req.environment,
            region=req.region,
            vpc=vpc,
            subnets=subnets,
            servers=list(servers),
            database=database,
            cache=cache,
            load_balancer=load_balancer,
            dns_records=registered,
            status="ready" if healthy == len(health_results) else "degraded",
        )

    def _check_cancel(self, checkpoint: str) -> None:
        if self._cancel_requested:
            raise ApplicationError(
                f"Provisioning cancelled at {checkpoint}",
                non_retryable=True,
            )


# ── Helpers ────────────────────────────────────────────────────────────────────

def _build_dns_records(
    env: str,
    servers: list[ServerResult],
    lb: Optional[LoadBalancerResult],
    db: Optional[DatabaseResult],
    cache: Optional[CacheResult],
) -> list[DnsRecord]:
    records: list[DnsRecord] = []

    entry_point = lb.dns_name if lb else servers[0].private_ip
    records.append(DnsRecord(name=f"{env}.internal", value=entry_point))

    for i, s in enumerate(servers):
        records.append(DnsRecord(name=f"server-{i}.{env}.internal", value=s.private_ip))

    if db:
        records.append(DnsRecord(name=f"db.{env}.internal", value=db.endpoint, record_type="CNAME"))

    if cache:
        records.append(DnsRecord(name=f"cache.{env}.internal", value=cache.endpoint, record_type="CNAME"))

    return records
