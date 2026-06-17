package template_runner

import (
	"fmt"
	"strings"
	"time"

	"go.temporal.io/sdk/workflow"
)

// isTerminal returns true when a node has reached a final (non-running) state.
func isTerminal(s NodeStatus) bool {
	return s == Completed || s == Skipped || s == Failed
}

// isFalsy checks whether a value from the input context should be treated as false/absent.
func isFalsy(v any) bool {
	if v == nil {
		return true
	}
	switch val := v.(type) {
	case bool:
		return !val
	case string:
		return val == "" || val == "false"
	case int:
		return val == 0
	case float64:
		return val == 0
	}
	return false
}

// resolveValue replaces a single "${ctx.field}" or "${node_id.field}" reference with its value.
// If the string is not a reference pattern it is returned unchanged.
func resolveValue(raw string, execContext map[string]any) any {
	if !strings.HasPrefix(raw, "${") || !strings.HasSuffix(raw, "}") {
		return raw
	}
	inner := raw[2 : len(raw)-1] // strip ${ and }
	dot := strings.Index(inner, ".")
	if dot < 0 {
		return raw
	}
	source := inner[:dot]
	field := inner[dot+1:]

	if source == "ctx" {
		if inputCtx, ok := execContext["_input"]; ok {
			if m, ok := inputCtx.(map[string]any); ok {
				if v, exists := m[field]; exists {
					return v
				}
			}
		}
		return nil
	}

	// node reference
	if nodeOut, ok := execContext[source]; ok {
		if m, ok := nodeOut.(map[string]any); ok {
			if v, exists := m[field]; exists {
				return v
			}
		}
	}
	return nil
}

// resolveParams replaces all parameter values that are reference strings with their resolved values.
func resolveParams(params map[string]any, execContext map[string]any) map[string]any {
	if params == nil {
		return map[string]any{}
	}
	resolved := make(map[string]any, len(params))
	for k, v := range params {
		if s, ok := v.(string); ok {
			resolved[k] = resolveValue(s, execContext)
		} else {
			resolved[k] = v
		}
	}
	return resolved
}

// snapshotGraph creates a copy of the current execution state for query handlers.
func snapshotGraph(templateID string, states map[string]*NodeState) ExecutionGraph {
	nodes := make(map[string]NodeState, len(states))
	for id, ns := range states {
		nodes[id] = *ns
	}
	return ExecutionGraph{
		TemplateID: templateID,
		Nodes:      nodes,
	}
}

// TemplateRunnerWorkflow interprets a Template DAG, executing each node as an activity
// while respecting dependency ordering, skip conditions, and cascading skips/failures.
func TemplateRunnerWorkflow(ctx workflow.Context, tmpl Template, inputCtx map[string]any) (map[string]any, error) {
	logger := workflow.GetLogger(ctx)

	// 1. Initialise all node states as Pending.
	states := make(map[string]*NodeState, len(tmpl.Nodes))
	for _, node := range tmpl.Nodes {
		states[node.ID] = &NodeState{
			NodeID:   node.ID,
			NodeName: node.Name,
			Status:   Pending,
		}
		if node.Condition != "" {
			states[node.ID].Condition = node.Condition
		}
	}

	// 2. Register the query handler so callers can inspect progress at any time.
	if err := workflow.SetQueryHandler(ctx, "get_execution_graph", func() (ExecutionGraph, error) {
		return snapshotGraph(tmpl.ID, states), nil
	}); err != nil {
		return nil, fmt.Errorf("failed to register query handler: %w", err)
	}

	// 3. Shared execution context: "_input" holds the caller-supplied context;
	//    completed node outputs are stored under their node ID.
	execContext := map[string]any{
		"_input": inputCtx,
	}

	// 5. Done channel — one token per node goroutine.
	doneCh := workflow.NewBufferedChannel(ctx, len(tmpl.Nodes))

	// 6. Fan-out: launch one coroutine per node.
	for _, node := range tmpl.Nodes {
		node := node // capture loop variable
		workflow.Go(ctx, func(gCtx workflow.Context) {
			state := states[node.ID]

			// a. Wait until every dependency has reached a terminal state.
			if len(node.DependsOn) > 0 {
				workflow.Await(gCtx, func() bool {
					for _, depID := range node.DependsOn {
						if dep, ok := states[depID]; !ok || !isTerminal(dep.Status) {
							return false
						}
					}
					return true
				})
			}

			// b. Determine whether this node should be skipped.
			skipReason := ""

			// Condition check
			if node.Condition != "" {
				val, exists := inputCtx[node.Condition]
				if !exists || isFalsy(val) {
					skipReason = fmt.Sprintf("condition %q is false or missing", node.Condition)
				}
			}

			// Cascade skip/fail from dependencies
			if skipReason == "" {
				for _, depID := range node.DependsOn {
					dep := states[depID]
					if dep.Status == Skipped {
						skipReason = fmt.Sprintf("dependency %q was skipped", depID)
						break
					}
					if dep.Status == Failed {
						skipReason = fmt.Sprintf("dependency %q failed", depID)
						break
					}
				}
			}

			if skipReason != "" {
				state.Status = Skipped
				state.SkipReason = skipReason
				logger.Info("Node skipped", "nodeID", node.ID, "reason", skipReason)
				doneCh.Send(gCtx, struct{}{})
				return
			}

			// c. Mark as running.
			state.Status = Running
			logger.Info("Node running", "nodeID", node.ID, "activity", node.Activity)

			// d. Resolve parameter references.
			resolvedParams := resolveParams(node.Params, execContext)

			// e. Execute the activity.
			ao := workflow.ActivityOptions{
				StartToCloseTimeout: 30 * time.Second,
			}
			actCtx := workflow.WithActivityOptions(gCtx, ao)

			var output map[string]any
			err := workflow.ExecuteActivity(actCtx, node.Activity, resolvedParams).Get(actCtx, &output)

			// f. Record outcome.
			if err != nil {
				state.Status = Failed
				state.Error = err.Error()
				logger.Info("Node failed", "nodeID", node.ID, "error", err)
			} else {
				state.Status = Completed
				state.Output = output
				execContext[node.ID] = output
				logger.Info("Node completed", "nodeID", node.ID)
			}

			doneCh.Send(gCtx, struct{}{})
		})
	}

	// 7. Wait for all goroutines to finish.
	for i := 0; i < len(tmpl.Nodes); i++ {
		doneCh.Receive(ctx, nil)
	}

	// 8. Build and return the final execution context (minus the internal "_input" key).
	result := make(map[string]any, len(execContext))
	for k, v := range execContext {
		if k != "_input" {
			result[k] = v
		}
	}
	return result, nil
}
