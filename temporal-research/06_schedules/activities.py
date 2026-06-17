import logging
from datetime import datetime, timezone

from temporalio import activity

logger = logging.getLogger(__name__)


@activity.defn
async def collect_metrics(environment: str) -> dict:
    """
    Simulate fetching metrics from an observability platform.
    In production: query Prometheus, Datadog, CloudWatch, etc.
    """
    logger.info("Collecting metrics for environment: %s", environment)
    # Simulated data — replace with real API calls
    return {
        "environment": environment,
        "timestamp": datetime.now(timezone.utc).isoformat(),
        "requests_per_minute": 1_240,
        "error_rate_pct": 0.4,
        "p99_latency_ms": 118,
    }


@activity.defn
async def publish_report(metrics: dict) -> str:
    """
    Deliver the report (email, Slack, S3 upload, database insert, etc.).
    """
    env = metrics.get("environment", "unknown")
    logger.info("Publishing report for %s", env)
    print(
        f"  [REPORT] {env} @ {metrics['timestamp']}"
        f" | rpm={metrics['requests_per_minute']}"
        f" | errors={metrics['error_rate_pct']}%"
        f" | p99={metrics['p99_latency_ms']}ms"
    )
    return f"Report published for {env}"
