# 05 — Child Workflows

Fan-out patterns: one parent orchestrates many concurrent child workflows.

## When to use child workflows vs. activities

| | Activity | Child Workflow |
|---|---|---|
| Duration | Seconds to minutes | Minutes to days |
| Has own state/signals/queries | No | Yes |
| Visible in UI independently | No | Yes |
| Retry granularity | Activity-level | Full workflow-level |
| Overhead | Low | Higher (one execution per child) |

Rule of thumb: if the work fits in one activity, use an activity. Use child
workflows when each unit is itself a complex, long-running, or independently
observable process.

## Fan-out pattern

```
BatchWorkflow
├── asyncio.gather(
│   ├── ItemWorkflow("img-1")  ──▶ process_item ──▶ {"item_id": "img-1", ...}
│   ├── ItemWorkflow("img-2")  ──▶ process_item ──▶ {"item_id": "img-2", ...}
│   ├── ...
│   └── ItemWorkflow("img-6")  ──▶ process_item ──▶ {"item_id": "img-6", ...}
│   )
└── aggregate_results ──▶ "Batch BATCH-001: 6/6 items completed"
```

Children run truly in parallel — each on whichever worker picks up its task.

## How to run

```bash
# Terminal 1
python worker.py

# Terminal 2
python starter.py
python starter.py MY-BATCH doc-a doc-b doc-c
```

Open the Web UI and navigate to `batch-BATCH-001` — you'll see the parent
workflow with links to each `BATCH-001-item-*` child execution.

## Child workflow lifecycle

- **Linked**: By default, if the parent is cancelled, children are cancelled.
- **Detached** (`parent_close_policy=ABANDON`): Children continue even if the
  parent workflow is terminated. Useful for fire-and-forget sub-processes.
- **Unique IDs**: Children need globally unique IDs. `{batch_id}-item-{item_id}`
  is a simple pattern. Re-using the same ID on a new batch will conflict — use
  a timestamp or UUID suffix if batches can overlap.
