"""
Pre-built template and contexts for the provisioning demo.

The template declares EVERY possible node.  The context decides which run.
Nodes with a condition whose key is absent / false are marked SKIPPED —
they still appear in the execution graph so the visualiser can show them.
"""
from models import Template, TemplateNode

# ── Template definition ───────────────────────────────────────────────────────

PROVISIONING_TEMPLATE = Template(
    id="cloud-infra-v1",
    name="Cloud Infrastructure Provisioning",
    description=(
        "Full environment: VPC, subnets, servers, optional DB/cache, "
        "load balancer, DNS, health checks."
    ),
    nodes=[
        # ── Level 0: network foundation ──────────────────────────────────────
        TemplateNode(
            id="vpc", name="Provision VPC",
            activity="provision_vpc",
            params={"region": "${ctx.region}"},
        ),

        # ── Level 1: subnets (parallel) ──────────────────────────────────────
        TemplateNode(
            id="subnet_a", name="Subnet A (az-a)",
            activity="provision_subnet",
            params={"vpc_id": "${vpc.vpc_id}", "az": "${ctx.region}a", "cidr": "10.0.0.0/24"},
            depends_on=["vpc"],
        ),
        TemplateNode(
            id="subnet_b", name="Subnet B (az-b)",
            activity="provision_subnet",
            params={"vpc_id": "${vpc.vpc_id}", "az": "${ctx.region}b", "cidr": "10.0.1.0/24"},
            depends_on=["vpc"],
        ),
        TemplateNode(
            id="subnet_c", name="Subnet C (az-c)",
            activity="provision_subnet",
            params={"vpc_id": "${vpc.vpc_id}", "az": "${ctx.region}c", "cidr": "10.0.2.0/24"},
            depends_on=["vpc"],
        ),

        # ── Level 2: compute + data (parallel, some conditional) ─────────────
        TemplateNode(
            id="server_1", name="Server 1",
            activity="provision_server",
            params={
                "vpc_id": "${vpc.vpc_id}", "subnet_id": "${subnet_a.subnet_id}",
                "az": "${ctx.region}a",   "instance_type": "${ctx.instance_type}",
            },
            depends_on=["subnet_a"],
        ),
        TemplateNode(
            id="server_2", name="Server 2",
            activity="provision_server",
            params={
                "vpc_id": "${vpc.vpc_id}", "subnet_id": "${subnet_b.subnet_id}",
                "az": "${ctx.region}b",   "instance_type": "${ctx.instance_type}",
            },
            depends_on=["subnet_b"],
            condition="multi_server",           # only if context says so
        ),
        TemplateNode(
            id="server_3", name="Server 3",
            activity="provision_server",
            params={
                "vpc_id": "${vpc.vpc_id}", "subnet_id": "${subnet_c.subnet_id}",
                "az": "${ctx.region}c",   "instance_type": "${ctx.instance_type}",
            },
            depends_on=["subnet_c"],
            condition="multi_server",
        ),
        TemplateNode(
            id="database", name="RDS Database",
            activity="provision_database",
            params={
                "vpc_id": "${vpc.vpc_id}", "subnet_id": "${subnet_a.subnet_id}",
                "instance_type": "${ctx.db_instance_type}",
            },
            depends_on=["subnet_a", "subnet_b", "subnet_c"],
            condition="include_database",
        ),
        TemplateNode(
            id="cache", name="ElastiCache Redis",
            activity="provision_cache",
            params={"vpc_id": "${vpc.vpc_id}", "subnet_id": "${subnet_a.subnet_id}"},
            depends_on=["subnet_a"],
            condition="include_cache",
        ),

        # ── Level 3: load balancer (conditional, needs multi-server) ─────────
        # Depends on server_2 → automatically SKIPPED when server_2 is skipped.
        # The condition also guards it explicitly for clarity.
        TemplateNode(
            id="load_balancer", name="Application Load Balancer",
            activity="provision_load_balancer",
            params={
                "vpc_id": "${vpc.vpc_id}", "environment": "${ctx.environment}",
                "server_1_id": "${server_1.server_id}",
                "server_2_id": "${server_2.server_id}",
            },
            depends_on=["server_1", "server_2"],
            condition="include_load_balancer",
        ),

        # ── Level 4: DNS (always, after at least server_1) ───────────────────
        TemplateNode(
            id="dns", name="Register DNS Records",
            activity="register_dns",
            params={
                "environment": "${ctx.environment}",
                "server_ip":   "${server_1.private_ip}",
                "lb_dns":      "${load_balancer.dns_name}",
            },
            depends_on=["server_1"],   # minimal hard dep; LB ref is best-effort
        ),

        # ── Level 5: health checks (parallel, each conditional on its resource)
        TemplateNode(
            id="health_server_1", name="Health: Server 1",
            activity="run_health_check",
            params={"resource_id": "${server_1.server_id}", "endpoint": "${server_1.private_ip}"},
            depends_on=["dns"],
        ),
        TemplateNode(
            id="health_server_2", name="Health: Server 2",
            activity="run_health_check",
            params={"resource_id": "${server_2.server_id}", "endpoint": "${server_2.private_ip}"},
            depends_on=["dns", "server_2"],
            condition="multi_server",
        ),
        TemplateNode(
            id="health_server_3", name="Health: Server 3",
            activity="run_health_check",
            params={"resource_id": "${server_3.server_id}", "endpoint": "${server_3.private_ip}"},
            depends_on=["dns", "server_3"],
            condition="multi_server",
        ),
        TemplateNode(
            id="health_database", name="Health: Database",
            activity="run_health_check",
            params={"resource_id": "${database.db_id}", "endpoint": "${database.endpoint}"},
            depends_on=["dns", "database"],
            condition="include_database",
        ),
        TemplateNode(
            id="health_cache", name="Health: Cache",
            activity="run_health_check",
            params={"resource_id": "${cache.cache_id}", "endpoint": "${cache.endpoint}"},
            depends_on=["dns", "cache"],
            condition="include_cache",
        ),
        TemplateNode(
            id="health_lb", name="Health: Load Balancer",
            activity="run_health_check",
            params={"resource_id": "${load_balancer.lb_id}", "endpoint": "${load_balancer.dns_name}"},
            depends_on=["dns", "load_balancer"],
            condition="include_load_balancer",
        ),
    ],
)


# ── Execution contexts ────────────────────────────────────────────────────────

CONTEXTS: dict[str, dict] = {
    "dev": {
        "environment":    "dev",
        "region":         "us-east-1",
        "instance_type":  "t3.small",
        "db_instance_type": "db.t3.micro",
        # multi_server, include_database, include_cache, include_load_balancer
        # are all absent → those nodes will be SKIPPED
    },
    "staging": {
        "environment":         "staging",
        "region":              "us-east-1",
        "instance_type":       "t3.medium",
        "db_instance_type":    "db.t3.small",
        "multi_server":        True,
        "include_database":    True,
        "include_load_balancer": True,
        # include_cache absent → cache + health_cache SKIPPED
    },
    "prod": {
        "environment":           "prod",
        "region":                "us-east-1",
        "instance_type":         "t3.large",
        "db_instance_type":      "db.t3.medium",
        "multi_server":          True,
        "include_database":      True,
        "include_cache":         True,
        "include_load_balancer": True,
    },
}
