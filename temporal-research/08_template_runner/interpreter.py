"""
Generic workflow interpreter: executes any Template definition.

Execution model
───────────────
Every node is launched as a concurrent coroutine immediately at workflow start.
Each coroutine internally waits (via workflow.wait_condition) until ALL its
declared dependencies reach a terminal state (completed / skipped / failed).

Once dependencies are terminal:
  1. Evaluate condition  → false or any dep was skipped/failed → SKIPPED
  2. Resolve ${} param references against context
  3. Dispatch the activity by name → RUNNING → COMPLETED or FAILED

asyncio.gather across all nodes gives maximum concurrency.
workflow.wait_condition is durable — the timer survives worker restarts.
"""
import asyncio
from datetime import timedelta
from typing import Any, Optional

from temporalio import workflow
from temporalio.exceptions import ApplicationError

with workflow.unsafe.imports_allowed():
    from models import NodeState, NodeStatus, Template, TemplateNode


_TIMEOUT  = timedelta(seconds=120)
_TERMINAL = frozenset({NodeStatus.COMPLETED, NodeStatus.SKIPPED, NodeStatus.FAILED})


# ── Param resolution ──────────────────────────────────────────────────────────

def _resolve(params: dict[str, Any], context: dict) -> dict:
    """
    Replace ${source.field} tokens with values from the execution context.

      ${ctx.region}          → context["_input"]["region"]
      ${subnet_a.subnet_id}  → context["subnet_a"]["subnet_id"]
    """
    out: dict[str, Any] = {}
    for key, value in params.items():
        if isinstance(value, str) and value.startswith("${") and value.endswith("}"):
            ref = value[2:-1]
            parts = ref.split(".", 1)
            src_key, field = parts[0], (parts[1] if len(parts) > 1 else None)
            src = context.get("_input", {}) if src_key == "ctx" else context.get(src_key, {})
            out[key] = src.get(field) if (field and isinstance(src, dict)) else src
        else:
            out[key] = value
    return out


# ── Interpreter workflow ──────────────────────────────────────────────────────

@workflow.defn
class TemplateRunnerWorkflow:

    def __init__(self) -> None:
        self._states: dict[str, NodeState] = {}
        self._cancelled = False

    # ── External interface ─────────────────────────────────────────────────

    @workflow.signal
    async def cancel(self) -> None:
        """Soft-cancel: marks all pending/running nodes as SKIPPED."""
        workflow.logger.info("Cancellation requested")
        self._cancelled = True

    @workflow.query
    def get_execution_graph(self) -> dict:
        """
        Returns the full node state map — including PENDING and SKIPPED nodes.
        This is the key difference from the Temporal event history: every
        declared node is always present, regardless of whether it ran.
        """
        return {
            "nodes": [
                {
                    "id":          s.node_id,
                    "name":        s.node_name,
                    "condition":   s.condition,
                    "status":      s.status.value,
                    "output":      s.output,
                    "error":       s.error,
                    "skip_reason": s.skip_reason,
                }
                for s in self._states.values()
            ]
        }

    # ── Entry point ────────────────────────────────────────────────────────

    @workflow.run
    async def run(self, template: Template, input_context: dict) -> dict:
        # Initialise every node as PENDING so the query returns the full graph
        # immediately — before any activity runs.
        for node in template.nodes:
            self._states[node.id] = NodeState(
                node_id=node.id,
                node_name=node.name,
                condition=node.condition,
            )

        # Embed input context so ${ctx.field} resolution works
        context: dict[str, Any] = {"_input": input_context}

        # Launch ALL nodes concurrently.  Each waits internally for its deps.
        await asyncio.gather(*[
            self._run_node(node, context) for node in template.nodes
        ])

        counts = {s.value: 0 for s in NodeStatus}
        for state in self._states.values():
            counts[state.status.value] += 1
        return counts

    # ── Per-node coroutine ─────────────────────────────────────────────────

    async def _run_node(self, node: TemplateNode, context: dict) -> None:
        # Step 1: wait until every declared dependency is terminal.
        # workflow.wait_condition is durable — the predicate is re-evaluated
        # after each activity / signal event, not on a timer.
        if node.depends_on:
            await workflow.wait_condition(
                lambda: all(
                    self._states[dep].status in _TERMINAL
                    for dep in node.depends_on
                )
            )

        if self._cancelled:
            self._set_skipped(node.id, "workflow cancelled")
            return

        # Step 2: decide whether to skip or execute.
        skip, reason = self._skip_reason(node, context)
        if skip:
            self._set_skipped(node.id, reason)
            return

        # Step 3: execute the activity.
        await self._execute(node, context)

    async def _execute(self, node: TemplateNode, context: dict) -> None:
        state = self._states[node.id]
        state.status = NodeStatus.RUNNING

        resolved = _resolve(node.params, context)

        try:
            output = await workflow.execute_activity(
                node.activity,          # dispatched by string name
                resolved,               # single dict arg
                start_to_close_timeout=_TIMEOUT,
            )
            state.status = NodeStatus.COMPLETED
            state.output = output if isinstance(output, dict) else {"result": output}
            # Publish output into context so downstream nodes can reference it
            context[node.id] = state.output
        except Exception as exc:
            state.status = NodeStatus.FAILED
            state.error   = str(exc)
            raise

    # ── Helpers ────────────────────────────────────────────────────────────

    def _skip_reason(self, node: TemplateNode, context: dict) -> tuple[bool, str]:
        # Explicit condition
        if node.condition is not None:
            if not context.get("_input", {}).get(node.condition):
                return True, f"condition '{node.condition}' = false"

        # Cascade: skip if any dependency was skipped or failed
        for dep_id in node.depends_on:
            dep = self._states.get(dep_id)
            if dep and dep.status == NodeStatus.SKIPPED:
                return True, f"upstream '{dep_id}' was skipped"
            if dep and dep.status == NodeStatus.FAILED:
                return True, f"upstream '{dep_id}' failed"

        return False, ""

    def _set_skipped(self, node_id: str, reason: str) -> None:
        s = self._states[node_id]
        s.status      = NodeStatus.SKIPPED
        s.skip_reason = reason
