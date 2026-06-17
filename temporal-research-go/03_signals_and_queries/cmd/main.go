package main

import (
	"context"
	"fmt"
	"log"
	"time"

	signals_and_queries "temporal-go/03_signals_and_queries"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

const taskQueue = "order-queue"

func main() {
	c, err := client.Dial(client.Options{})
	if err != nil {
		log.Fatal("Unable to create Temporal client:", err)
	}
	defer c.Close()

	w := worker.New(c, taskQueue, worker.Options{})
	w.RegisterWorkflow(signals_and_queries.OrderWorkflow)
	w.RegisterActivity(signals_and_queries.ProcessPayment)

	go func() {
		if err := w.Run(worker.InterruptCh()); err != nil {
			log.Fatal("Worker error:", err)
		}
	}()

	we, err := c.ExecuteWorkflow(context.Background(), client.StartWorkflowOptions{
		ID:        "order-workflow-123",
		TaskQueue: taskQueue,
	}, signals_and_queries.OrderWorkflow, "order-123", 99.99)
	if err != nil {
		log.Fatal("Unable to start workflow:", err)
	}

	// After 1 second, send the payment_received signal
	go func() {
		time.Sleep(1 * time.Second)
		err := c.SignalWorkflow(context.Background(), we.GetID(), we.GetRunID(), "payment_received", "tx-abc-123")
		if err != nil {
			log.Println("Failed to send signal:", err)
		} else {
			fmt.Println("Signal sent: payment_received tx-abc-123")
		}
	}()

	var result string
	if err := we.Get(context.Background(), &result); err != nil {
		log.Fatal("Workflow failed:", err)
	}

	fmt.Println("Result:", result)
}
