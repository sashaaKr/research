# Temporal Research — Go

Progressive Temporal SDK examples in Go, mirroring the Python examples in `../temporal-research/`.

## Prerequisites

- Go 1.22+
- Temporal server running on `localhost:7233` — use the docker-compose from the sibling folder:
  ```bash
  cd ../temporal-research && docker compose up -d
  ```

## Running examples

```bash
# Download dependencies (first time only)
go mod tidy

# Run any example
go run ./01_hello_world/cmd/
go run ./02_retry_and_errors/cmd/
go run ./03_signals_and_queries/cmd/
go run ./04_timers_and_sleep/cmd/
go run ./05_child_workflows/cmd/
go run ./06_schedules/cmd/
go run ./07_provisioning/cmd/
go run ./08_template_runner/cmd/
```

Each `cmd/main.go` starts an in-process worker **and** submits a workflow, so a single command gives you the full demo. Workflow + activity code lives in the parent package; `cmd/` holds only the entry point.

## Examples

| # | Example | Temporal concepts |
|---|---------|-------------------|
| 01 | Hello World | Workflow, Activity, Worker, TaskQueue |
| 02 | Retry & Errors | RetryPolicy, ApplicationError, non-retryable errors |
| 03 | Signals & Queries | GetSignalChannel, SetQueryHandler, NewSelector |
| 04 | Timers & Sleep | workflow.Sleep, durable timers |
| 05 | Child Workflows | ExecuteChildWorkflow, fan-out parallelism with workflow.Go |
| 06 | Schedules | ScheduleClient, cron expressions |
| 07 | Provisioning | Multi-phase workflow, parallel activities + child workflows, conditional branches |
| 08 | Template Runner | Generic DAG interpreter, workflow.Await for dep-waiting, full execution graph query |

## Go SDK vs Python SDK

| Concept | Python | Go |
|---------|--------|----|
| Async execution | `asyncio.gather` | `workflow.Go` + channel |
| Wait for condition | `workflow.wait_condition(lambda)` | `workflow.Await(ctx, func() bool)` |
| Signal receive | `workflow.wait_condition` + handler | `workflow.GetSignalChannel` + `Select` |
| Activity call | `await workflow.execute_activity(...)` | `workflow.ExecuteActivity(...).Get(ctx, &result)` |
| Child workflow | `await workflow.execute_child_workflow(...)` | `workflow.ExecuteChildWorkflow(...).Get(ctx, &result)` |
| Non-retryable err | `ApplicationError(non_retryable=True)` | `temporal.NewNonRetryableApplicationError(...)` |

## Module structure

One Go module (`go.mod`) at the root; each example is a `package main` in its own directory. Build independently with `go run ./NN_example/` or `go build ./NN_example/`.
