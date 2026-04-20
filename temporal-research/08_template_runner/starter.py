"""
Run a template and stream the full execution graph to the terminal.

Usage:
    python starter.py [dev|staging|prod] [--cancel]

The graph re-renders every 400ms and shows EVERY declared node:
  PENDING   — not yet ready to run
  RUNNING   — activity in flight
  COMPLETED — done, output shown
  SKIPPED   — condition was false or an upstream dep was skipped  ← explicit!
  FAILED    — activity error
"""
import asyncio
import os
import sys

from temporalio.client import Client
from temporalio.exceptions import WorkflowFailureError

from interpreter import TemplateRunnerWorkflow
from models import Template, TemplateNode
from templates import CONTEXTS, PROVISIONING_TEMPLATE

TASK_QUEUE = "template-runner-queue"

# ── Terminal colours ──────────────────────────────────────────────────────────

_C = {
    "pending":   "\033[90m",    # dark grey
    "running":   "\033[93m",    # yellow
    "completed": "\033[92m",    # green
    "skipped":   "\033[96m",    # cyan  ← stands out, not an error
    "failed":    "\033[91m",    # red
}
_BOLD  = "\033[1m"
_DIM   = "\033[2m"
_RESET = "\033[0m"

_ICON = {
    "pending":   "○",
    "running":   "◉",
    "completed": "✓",
    "skipped":   "⊘",
    "failed":    "✗",
}


# ── Topology helpers ──────────────────────────────────────────────────────────

def _topo_levels(template: Template) -> dict[str, int]:
    """Compute the topological depth of each node for grouped display."""
    node_map = {n.id: n for n in template.nodes}
    cache: dict[str, int] = {}

    def depth(node_id: str) -> int:
        if node_id in cache:
            return cache[node_id]
        node = node_map[node_id]
        cache[node_id] = (
            0 if not node.depends_on
            else max(depth(d) for d in node.depends_on) + 1
        )
        return cache[node_id]

    for node in template.nodes:
        depth(node.id)
    return cache


# ── Renderer ──────────────────────────────────────────────────────────────────

def _render(template: Template, graph: dict, scenario: str, levels: dict[str, int]) -> str:
    node_states = {n["id"]: n for n in graph["nodes"]}
    max_level   = max(levels.values(), default=0)
    lines: list[str] = []

    # Header
    lines.append(f"{_BOLD}{template.name}{_RESET}  {_DIM}[{scenario}]{_RESET}")
    lines.append("─" * 72)

    for level in range(max_level + 1):
        level_nodes = [n for n in template.nodes if levels[n.id] == level]

        if level > 0:
            lines.append(f"  {_DIM}│{_RESET}")

        for node in level_nodes:
            nd     = node_states.get(node.id, {})
            status = nd.get("status", "pending")
            color  = _C.get(status, "")
            icon   = _ICON.get(status, "?")

            # Condition hint
            cond_hint = (
                f" {_DIM}[if {node.condition}]{_RESET}"
                if node.condition else ""
            )

            # Detail: output summary or skip reason
            detail = ""
            if status == "completed":
                out = nd.get("output") or {}
                # Show first non-trivial field from output
                first = next(
                    (f"{k}={v}" for k, v in out.items()
                     if k not in ("region",) and v),
                    None,
                )
                if first:
                    detail = f"  {_DIM}{first}{_RESET}"
            elif status == "skipped":
                reason = nd.get("skip_reason", "")
                detail = f"  {_DIM}↳ {reason}{_RESET}"
            elif status == "failed":
                detail = f"  {_C['failed']}{nd.get('error','')[:60]}{_RESET}"

            name_col = f"{node.name}{cond_hint}"
            lines.append(
                f"  {color}{icon}{_RESET} "
                f"{name_col:<42}"
                f"{color}{status.upper():<11}{_RESET}"
                f"{detail}"
            )

    lines.append("─" * 72)

    # Summary counts
    counts: dict[str, int] = {s: 0 for s in _C}
    for nd in graph["nodes"]:
        counts[nd.get("status", "pending")] += 1

    parts = [
        f"{_C[s]}{s.upper()} {counts[s]}{_RESET}"
        for s in ("completed", "skipped", "running", "pending", "failed")
        if counts[s] > 0
    ]
    lines.append("  ".join(parts))

    return "\n".join(lines)


# ── Streaming loop ────────────────────────────────────────────────────────────

async def stream(handle, template: Template, scenario: str, levels: dict) -> None:
    terminal_statuses = {"completed", "skipped", "failed"}

    while True:
        try:
            graph = await handle.query(TemplateRunnerWorkflow.get_execution_graph)
        except Exception:
            return

        # Clear terminal and redraw
        os.system("clear" if os.name != "nt" else "cls")
        print(_render(template, graph, scenario, levels))

        all_done = all(
            n["status"] in terminal_statuses for n in graph["nodes"]
        )
        if all_done:
            return

        await asyncio.sleep(0.4)


# ── Entry point ───────────────────────────────────────────────────────────────

async def main(scenario: str, send_cancel: bool) -> None:
    if scenario not in CONTEXTS:
        print(f"Unknown scenario {scenario!r}. Available: {', '.join(CONTEXTS)}")
        sys.exit(1)

    template = PROVISIONING_TEMPLATE
    context  = CONTEXTS[scenario]
    levels   = _topo_levels(template)

    client = await Client.connect("localhost:7233")

    handle = await client.start_workflow(
        TemplateRunnerWorkflow.run,
        args=[template, context],
        id=f"template-{template.id}-{scenario}",
        task_queue=TASK_QUEUE,
    )

    if send_cancel:
        await asyncio.sleep(1.5)
        await handle.signal(TemplateRunnerWorkflow.cancel)

    await stream(handle, template, scenario, levels)

    try:
        summary = await handle.result()
        print(f"\nSummary: {summary}")
    except WorkflowFailureError as e:
        print(f"\nWorkflow failed: {e.__cause__}")


if __name__ == "__main__":
    scenario    = next((a for a in sys.argv[1:] if not a.startswith("-")), "dev")
    send_cancel = "--cancel" in sys.argv
    asyncio.run(main(scenario, send_cancel))
