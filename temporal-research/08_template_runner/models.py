from __future__ import annotations

from dataclasses import dataclass, field
from enum import Enum
from typing import Any, Optional


class NodeStatus(str, Enum):
    PENDING   = "pending"
    RUNNING   = "running"
    COMPLETED = "completed"
    SKIPPED   = "skipped"   # condition was false, or an upstream dep was skipped/failed
    FAILED    = "failed"


@dataclass
class TemplateNode:
    """
    A single step in a template.

    params supports ${source.field} references:
      ${ctx.field}      → value from the input context
      ${node_id.field}  → field from a completed node's output dict
    """
    id: str
    name: str
    activity: str                             # dispatched by string name to the worker
    params: dict[str, Any] = field(default_factory=dict)
    depends_on: list[str] = field(default_factory=list)
    condition: Optional[str] = None           # context key; falsy / absent → SKIPPED


@dataclass
class Template:
    id: str
    name: str
    description: str
    nodes: list[TemplateNode]


@dataclass
class NodeState:
    node_id: str
    node_name: str
    condition: Optional[str] = None           # carried for the visualiser
    status: NodeStatus = NodeStatus.PENDING
    output: Optional[dict] = None
    error: Optional[str] = None
    skip_reason: Optional[str] = None
