package child_workflows

import (
	"context"
	"fmt"
	"time"
)

func ProcessItem(ctx context.Context, itemID string) (string, error) {
	time.Sleep(1 * time.Second)
	return fmt.Sprintf("processed: %s", itemID), nil
}
