package retry_and_errors

import (
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

func RetryDemoWorkflow(ctx workflow.Context) (string, error) {
	logger := workflow.GetLogger(ctx)

	// Run FlakyActivity with retry policy
	flakyOpts := workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    500 * time.Millisecond,
			BackoffCoefficient: 2.0,
			MaximumAttempts:    5,
		},
	}
	flakyCtx := workflow.WithActivityOptions(ctx, flakyOpts)

	var flakyResult string
	err := workflow.ExecuteActivity(flakyCtx, FlakyActivity).Get(flakyCtx, &flakyResult)
	if err != nil {
		logger.Info("FlakyActivity failed after retries", "error", err)
		return "", fmt.Errorf("flaky activity failed: %w", err)
	}
	logger.Info("FlakyActivity succeeded", "result", flakyResult)

	// Run RiskyActivity with shouldFail=false
	riskyOpts := workflow.ActivityOptions{
		StartToCloseTimeout: 10 * time.Second,
	}
	riskyCtx := workflow.WithActivityOptions(ctx, riskyOpts)

	var riskyResult string
	err = workflow.ExecuteActivity(riskyCtx, RiskyActivity, false).Get(riskyCtx, &riskyResult)
	if err != nil {
		logger.Info("RiskyActivity(false) failed unexpectedly", "error", err)
		return "", fmt.Errorf("risky activity (no-fail) failed: %w", err)
	}
	logger.Info("RiskyActivity(false) result", "result", riskyResult)

	// Run RiskyActivity with shouldFail=true — expect a non-retryable error, catch and continue
	var fatalResult string
	err = workflow.ExecuteActivity(riskyCtx, RiskyActivity, true).Get(riskyCtx, &fatalResult)
	if err != nil {
		logger.Info("RiskyActivity(true) returned expected non-retryable error", "error", err.Error())
	} else {
		logger.Info("RiskyActivity(true) unexpectedly succeeded", "result", fatalResult)
	}

	summary := fmt.Sprintf("flaky=%q risky_safe=%q risky_fatal=caught", flakyResult, riskyResult)
	return summary, nil
}
