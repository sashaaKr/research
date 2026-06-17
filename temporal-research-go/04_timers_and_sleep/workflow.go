package timers_and_sleep

import (
	"context"
	"fmt"
	"time"

	"go.temporal.io/sdk/workflow"
)

var checkCount int

func CheckStatus(ctx context.Context, itemID string) (string, error) {
	checkCount++
	if checkCount >= 3 {
		return "ready", nil
	}
	return "pending", nil
}

func CheckInWorkflow(ctx workflow.Context, itemID string) (string, error) {
	logger := workflow.GetLogger(ctx)

	ao := workflow.ActivityOptions{
		StartToCloseTimeout: 10 * time.Second,
	}
	actCtx := workflow.WithActivityOptions(ctx, ao)

	maxChecks := 3
	var lastStatus string

	for i := 0; i < maxChecks; i++ {
		var status string
		err := workflow.ExecuteActivity(actCtx, CheckStatus, itemID).Get(actCtx, &status)
		if err != nil {
			return "", fmt.Errorf("check %d failed: %w", i+1, err)
		}

		lastStatus = status
		logger.Info("Status check", "check", i+1, "status", status, "itemID", itemID)

		if status == "ready" {
			logger.Info("Item is ready, returning early", "itemID", itemID)
			return fmt.Sprintf("item %s is ready after %d checks", itemID, i+1), nil
		}

		if i < maxChecks-1 {
			if err := workflow.Sleep(ctx, 2*time.Second); err != nil {
				return "", fmt.Errorf("sleep interrupted: %w", err)
			}
		}
	}

	return fmt.Sprintf("item %s final status: %s after %d checks", itemID, lastStatus, maxChecks), nil
}
