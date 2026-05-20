package child_workflows

import (
	"fmt"
	"time"

	"go.temporal.io/sdk/workflow"
)

func ItemWorkflow(ctx workflow.Context, itemID string) (string, error) {
	ao := workflow.ActivityOptions{
		StartToCloseTimeout: 10 * time.Second,
	}
	actCtx := workflow.WithActivityOptions(ctx, ao)

	var result string
	err := workflow.ExecuteActivity(actCtx, ProcessItem, itemID).Get(actCtx, &result)
	if err != nil {
		return "", fmt.Errorf("ProcessItem failed for %s: %w", itemID, err)
	}
	return result, nil
}

type itemResult struct {
	value string
	err   error
}

func BatchWorkflow(ctx workflow.Context, items []string) ([]string, error) {
	resultCh := workflow.NewBufferedChannel(ctx, len(items))

	for _, item := range items {
		itemID := item
		workflow.Go(ctx, func(gCtx workflow.Context) {
			cwo := workflow.ChildWorkflowOptions{
				WorkflowExecutionTimeout: 30 * time.Second,
			}
			childCtx := workflow.WithChildOptions(gCtx, cwo)

			var res string
			err := workflow.ExecuteChildWorkflow(childCtx, ItemWorkflow, itemID).Get(childCtx, &res)
			resultCh.Send(gCtx, itemResult{value: res, err: err})
		})
	}

	results := make([]string, 0, len(items))
	for i := 0; i < len(items); i++ {
		var r itemResult
		resultCh.Receive(ctx, &r)
		if r.err != nil {
			results = append(results, fmt.Sprintf("error: %v", r.err))
		} else {
			results = append(results, r.value)
		}
	}

	return results, nil
}
