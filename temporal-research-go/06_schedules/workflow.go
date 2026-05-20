package schedules

import (
	"context"
	"fmt"
	"time"

	"go.temporal.io/sdk/workflow"
)

func GenerateReport(ctx context.Context, reportType string) (string, error) {
	timestamp := time.Now().UTC().Format(time.RFC3339)
	return fmt.Sprintf("report:%s:%s", reportType, timestamp), nil
}

func ReportWorkflow(ctx workflow.Context, reportType string) (string, error) {
	logger := workflow.GetLogger(ctx)

	now := workflow.Now(ctx)
	logger.Info("ReportWorkflow started", "reportType", reportType, "time", now)

	ao := workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
	}
	actCtx := workflow.WithActivityOptions(ctx, ao)

	var result string
	err := workflow.ExecuteActivity(actCtx, GenerateReport, reportType).Get(actCtx, &result)
	if err != nil {
		return "", fmt.Errorf("GenerateReport failed: %w", err)
	}

	logger.Info("Report generated", "result", result)
	return result, nil
}
