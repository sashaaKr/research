"""
TemplateRunnerWorkflow — extended from 08_template_runner for K8s deployments.

The key addition: `activity_task_queue` parameter on `run()`.

In K8s we deploy two separate worker Deployments:
  orchestrator  → polls  "template-runner-queue"  (workflow tasks)
  activities    → polls  "provisioning-queue"      (activity tasks)

The orchestrator workflow receives `activity_task_queue="provisioning-queue"`
and forwards it to every execute_activity() call, routing activity tasks to the
activities Deployment instead of the orchestrator Deployment.

Without this, the orchestrator pods would need to be sized for both workflow
orchestration AND heavy activity execution — losing the ability to scale them
independently.
"""
import asyncio
from datetime import timedelta
from typing import Any, Optional

from temporalio import workflow

with workflow.unsafe.imports_passed_through():
    from models import NodeState, NodeStatus, Template, TemplateNode


_TIMEOUT  = timedelta(seconds=120)
_TERMINAL = frozenset({NodeStatus.COMPLETED, NodeStatus.SKIPPED, NodeStatus.FAILED})


def _resolve(params: dict[str, Any], context: dict) -> dict:
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


@workflow.defn
class TemplateRunnerWorkflow:

    def __init__(self) -> None:
        self._states: dict[str, NodeState] = {}
        self._cancelled = False
        self._activity_task_queue: Optional[str] = None

    @workflow.signal
    async def cancel(self) -> None:
        self._cancelled = True

    @workflow.query
    def get_execution_graph(self) -> dict:
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

    @workflow.run
    async def run(
        self,
        template: Template,
        input_context: dict,
        activity_task_queue: Optional[str] = None,
    ) -> dict:
        # Store so _execute() can reference it without passing through every call
        self._activity_task_queue = activity_task_queue

        for node in template.nodes:
            self._states[node.id] = NodeState(
                node_id=node.id,
                node_name=node.name,
                condition=node.condition,
            )

        context: dict[str, Any] = {"_input": input_context}

        await asyncio.gather(*[
            self._run_node(node, context) for node in template.nodes
        ])

        counts = {s.value: 0 for s in NodeStatus}
        for state in self._states.values():
            counts[state.status.value] += 1
        return counts

    async def _run_node(self, node: TemplateNode, context: dict) -> None:
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

        skip, reason = self._skip_reason(node, context)
        if skip:
            self._set_skipped(node.id, reason)
            return

        await self._execute(node, context)

    async def _execute(self, node: TemplateNode, context: dict) -> None:
        state = self._states[node.id]
        state.status = NodeStatus.RUNNING

        resolved = _resolve(node.params, context)

        # Route activity tasks to the dedicated activity worker task queue.
        # Falls back to the workflow's own task queue when not set (local dev).
        kwargs: dict[str, Any] = {"start_to_close_timeout": _TIMEOUT}
        if self._activity_task_queue:
            kwargs["task_queue"] = self._activity_task_queue

        try:
            output = await workflow.execute_activity(node.activity, resolved, **kwargs)
            state.status = NodeStatus.COMPLETED
            state.output = output if isinstance(output, dict) else {"result": output}
            context[node.id] = state.output
        except Exception as exc:
            state.status = NodeStatus.FAILED
            state.error   = str(exc)
            raise

    def _skip_reason(self, node: TemplateNode, context: dict) -> tuple[bool, str]:
        if node.condition is not None:
            if not context.get("_input", {}).get(node.condition):
                return True, f"condition '{node.condition}' = false"

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
