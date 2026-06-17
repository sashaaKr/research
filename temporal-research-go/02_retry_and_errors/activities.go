package retry_and_errors

import (
	"context"
	"fmt"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
)

// FlakyActivity fails on the first 3 attempts and succeeds on the 4th.
// Temporal tracks the attempt number via the activity context.
func FlakyActivity(ctx context.Context) (string, error) {
	info := activity.GetInfo(ctx)
	attempt := info.Attempt
	if attempt < 4 {
		return "", fmt.Errorf("transient: attempt %d", attempt)
	}
	return fmt.Sprintf("success on attempt %d", attempt), nil
}

func RiskyActivity(ctx context.Context, shouldFail bool) (string, error) {
	if shouldFail {
		return "", temporal.NewNonRetryableApplicationError("fatal: will not retry", "FatalError", nil)
	}
	return "success", nil
}
