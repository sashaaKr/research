"""
All activities accept a single `params: dict` and return a `dict`.

This uniform interface lets the interpreter dispatch any activity by name
without knowing its type signature — it just passes the resolved param dict
and stores whatever dict comes back.
"""
import asyncio
import logging
import random
import uuid
from typing import Any

from temporalio import activity

logger = logging.getLogger(__name__)


def _id(prefix: str) -> str:
    return f"{prefix}-{uuid.uuid4().hex[:8]}"


@activity.defn
async def provision_vpc(params: dict[str, Any]) -> dict:
    region = params.get("region", "us-east-1")
    logger.info("Provisioning VPC in %s", region)
    await asyncio.sleep(0.4)
    return {"vpc_id": _id("vpc"), "cidr": "10.0.0.0/16", "region": region}


@activity.defn
async def provision_subnet(params: dict[str, Any]) -> dict:
    logger.info("Provisioning subnet in %s", params.get("az"))
    await asyncio.sleep(0.3)
    return {
        "subnet_id": _id("subnet"),
        "vpc_id": params["vpc_id"],
        "az": params["az"],
        "cidr": params.get("cidr", "10.0.0.0/24"),
    }


@activity.defn
async def provision_server(params: dict[str, Any]) -> dict:
    logger.info("Provisioning %s in %s", params.get("instance_type", "t3.medium"), params.get("az"))
    await asyncio.sleep(random.uniform(0.6, 1.2))
    return {
        "server_id": _id("i"),
        "private_ip": f"10.0.{random.randint(1, 254)}.{random.randint(2, 254)}",
        "az": params.get("az"),
        "instance_type": params.get("instance_type", "t3.medium"),
    }


@activity.defn
async def provision_database(params: dict[str, Any]) -> dict:
    logger.info("Provisioning RDS (%s)", params.get("instance_type", "db.t3.small"))
    await asyncio.sleep(1.4)
    db_id = _id("db")
    return {
        "db_id": db_id,
        "endpoint": f"{db_id}.cluster.rds.amazonaws.com",
        "port": 5432,
        "instance_type": params.get("instance_type", "db.t3.small"),
    }


@activity.defn
async def provision_cache(params: dict[str, Any]) -> dict:
    logger.info("Provisioning ElastiCache")
    await asyncio.sleep(0.9)
    cache_id = _id("cache")
    return {
        "cache_id": cache_id,
        "endpoint": f"{cache_id}.cache.amazonaws.com",
        "port": 6379,
    }


@activity.defn
async def provision_load_balancer(params: dict[str, Any]) -> dict:
    logger.info("Provisioning ALB")
    await asyncio.sleep(0.8)
    lb_id = _id("alb")
    env = params.get("environment", "env")
    return {
        "lb_id": lb_id,
        "dns_name": f"{env}.{lb_id}.elb.amazonaws.com",
    }


@activity.defn
async def register_dns(params: dict[str, Any]) -> dict:
    logger.info("Registering DNS for %s", params.get("environment"))
    await asyncio.sleep(0.3)
    env = params.get("environment", "env")
    entry = params.get("lb_dns") or params.get("server_ip", "0.0.0.0")
    return {
        "zone": f"{env}.internal",
        "records": [
            {"name": f"{env}.internal",          "value": entry},
            {"name": f"api.{env}.internal",      "value": entry},
        ],
    }


@activity.defn
async def run_health_check(params: dict[str, Any]) -> dict:
    resource_id = params.get("resource_id", "unknown")
    endpoint    = params.get("endpoint", "")
    logger.info("Health check: %s @ %s", resource_id, endpoint)
    await asyncio.sleep(random.uniform(0.1, 0.3))
    return {"resource_id": resource_id, "endpoint": endpoint, "healthy": True}
