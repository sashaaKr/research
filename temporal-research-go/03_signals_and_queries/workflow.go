package signals_and_queries

import (
	"context"
	"fmt"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/workflow"
)

type OrderStatus struct {
	State   string
	OrderID string
	Amount  float64
}

func ProcessPayment(ctx context.Context, orderID, txID string) (string, error) {
	activity.RecordHeartbeat(ctx, "processing payment")
	time.Sleep(1 * time.Second)
	return fmt.Sprintf("shipped: %s", orderID), nil
}

func OrderWorkflow(ctx workflow.Context, orderID string, amount float64) (string, error) {
	logger := workflow.GetLogger(ctx)

	status := OrderStatus{
		State:   "waiting_payment",
		OrderID: orderID,
		Amount:  amount,
	}

	err := workflow.SetQueryHandler(ctx, "get_status", func() (OrderStatus, error) {
		return status, nil
	})
	if err != nil {
		return "", fmt.Errorf("failed to register query handler: %w", err)
	}

	paymentCh := workflow.GetSignalChannel(ctx, "payment_received")
	cancelCh := workflow.GetSignalChannel(ctx, "cancel_order")
	timer := workflow.NewTimer(ctx, 5*time.Minute)

	var result string
	selector := workflow.NewSelector(ctx)

	selector.AddReceive(paymentCh, func(ch workflow.ReceiveChannel, more bool) {
		var txID string
		ch.Receive(ctx, &txID)
		status.State = "payment_received"
		logger.Info("Payment received", "orderID", orderID, "txID", txID)

		ao := workflow.ActivityOptions{
			StartToCloseTimeout: 30 * time.Second,
		}
		actCtx := workflow.WithActivityOptions(ctx, ao)

		var shipResult string
		if err := workflow.ExecuteActivity(actCtx, ProcessPayment, orderID, txID).Get(actCtx, &shipResult); err != nil {
			logger.Info("ProcessPayment failed", "error", err)
			result = fmt.Sprintf("payment error: %v", err)
			return
		}
		status.State = "shipped"
		result = shipResult
	})

	selector.AddReceive(cancelCh, func(ch workflow.ReceiveChannel, more bool) {
		var reason string
		ch.Receive(ctx, &reason)
		status.State = "cancelled"
		logger.Info("Order cancelled", "orderID", orderID, "reason", reason)
		result = "order cancelled"
	})

	selector.AddFuture(timer, func(f workflow.Future) {
		status.State = "expired"
		logger.Info("Order expired", "orderID", orderID)
		result = "order expired"
	})

	selector.Select(ctx)
	return result, nil
}
