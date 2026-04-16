from dataclasses import dataclass, field
from typing import Optional


@dataclass
class ProvisioningRequest:
    environment: str         # "dev" | "staging" | "prod"
    region: str              # e.g. "us-east-1"
    server_count: int
    instance_type: str = "t3.medium"
    include_database: bool = False
    db_instance_type: str = "db.t3.small"
    include_cache: bool = False


@dataclass
class VpcResult:
    vpc_id: str
    cidr: str


@dataclass
class SubnetResult:
    subnet_id: str
    availability_zone: str
    cidr: str


@dataclass
class ServerResult:
    server_id: str
    private_ip: str
    availability_zone: str
    instance_type: str


@dataclass
class DatabaseResult:
    db_id: str
    endpoint: str
    port: int = 5432


@dataclass
class CacheResult:
    cache_id: str
    endpoint: str
    port: int = 6379


@dataclass
class LoadBalancerResult:
    lb_id: str
    dns_name: str


@dataclass
class DnsRecord:
    name: str
    value: str
    record_type: str = "A"


@dataclass
class EnvironmentManifest:
    environment: str
    region: str
    vpc: VpcResult
    subnets: list[SubnetResult]
    servers: list[ServerResult]
    dns_records: list[DnsRecord]
    status: str = "ready"
    database: Optional[DatabaseResult] = None
    cache: Optional[CacheResult] = None
    load_balancer: Optional[LoadBalancerResult] = None
