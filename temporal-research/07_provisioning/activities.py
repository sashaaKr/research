import asyncio
import logging
import random
import uuid
from typing import Optional

from temporalio import activity

from models import (
    CacheResult,
    DatabaseResult,
    DnsRecord,
    LoadBalancerResult,
    ServerResult,
    SubnetResult,
    VpcResult,
)

logger = logging.getLogger(__name__)


def _id(prefix: str) -> str:
    return f"{prefix}-{uuid.uuid4().hex[:8]}"


@activity.defn
async def provision_vpc(region: str) -> VpcResult:
    logger.info("Provisioning VPC in %s", region)
    await asyncio.sleep(0.4)
    return VpcResult(vpc_id=_id("vpc"), cidr="10.0.0.0/16")


@activity.defn
async def provision_subnet(vpc_id: str, az: str, cidr: str) -> SubnetResult:
    logger.info("Provisioning subnet %s in %s", cidr, az)
    await asyncio.sleep(0.3)
    return SubnetResult(subnet_id=_id("subnet"), availability_zone=az, cidr=cidr)


@activity.defn
async def provision_server(
    vpc_id: str, subnet_id: str, instance_type: str, az: str, environment: str
) -> ServerResult:
    logger.info("Provisioning %s server in %s", instance_type, az)
    await asyncio.sleep(random.uniform(0.8, 1.5))  # servers take variable time
    return ServerResult(
        server_id=_id("i"),
        private_ip=f"10.0.{random.randint(1, 254)}.{random.randint(2, 254)}",
        availability_zone=az,
        instance_type=instance_type,
    )


@activity.defn
async def provision_database(
    vpc_id: str, subnet_ids: list[str], instance_type: str, environment: str
) -> DatabaseResult:
    logger.info("Provisioning RDS (%s) in %s", instance_type, vpc_id)
    await asyncio.sleep(1.5)  # databases take longer
    db_id = _id("db")
    return DatabaseResult(db_id=db_id, endpoint=f"{db_id}.cluster.rds.amazonaws.com")


@activity.defn
async def provision_cache(
    vpc_id: str, subnet_id: str, environment: str
) -> CacheResult:
    logger.info("Provisioning ElastiCache in %s", vpc_id)
    await asyncio.sleep(1.0)
    cache_id = _id("cache")
    return CacheResult(cache_id=cache_id, endpoint=f"{cache_id}.cache.amazonaws.com")


@activity.defn
async def provision_load_balancer(
    vpc_id: str, subnet_ids: list[str], server_ids: list[str], environment: str
) -> LoadBalancerResult:
    logger.info("Provisioning ALB for %d servers", len(server_ids))
    await asyncio.sleep(0.8)
    lb_id = _id("alb")
    return LoadBalancerResult(
        lb_id=lb_id, dns_name=f"{environment}.{lb_id}.elb.amazonaws.com"
    )


@activity.defn
async def register_dns_records(
    records: list[DnsRecord], environment: str
) -> list[DnsRecord]:
    logger.info("Registering %d DNS records for %s", len(records), environment)
    await asyncio.sleep(0.3)
    return records


@activity.defn
async def run_health_check(resource_id: str, endpoint: str) -> dict:
    logger.info("Health check: %s @ %s", resource_id, endpoint)
    await asyncio.sleep(random.uniform(0.1, 0.3))
    return {"resource_id": resource_id, "endpoint": endpoint, "healthy": True}
