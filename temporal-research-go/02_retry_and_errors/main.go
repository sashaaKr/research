package main

import (
	"context"
	"fmt"
	"log"

	retry_and_errors "temporal-go/02_retry_and_errors"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

const taskQueue = "retry-queue"

func main() {
	c, err := client.Dial(client.Options{})
	if err != nil {
		log.Fatal("Unable to create Temporal client:", err)
	}
	defer c.Close()

	w := worker.New(c, taskQueue, worker.Options{})
	w.RegisterWorkflow(retry_and_errors.RetryDemoWorkflow)
	w.RegisterActivity(retry_and_errors.FlakyActivity)
	w.RegisterActivity(retry_and_errors.RiskyActivity)

	go func() {
		if err := w.Run(worker.InterruptCh()); err != nil {
			log.Fatal("Worker error:", err)
		}
	}()

	we, err := c.ExecuteWorkflow(context.Background(), client.StartWorkflowOptions{
		ID:        "retry-demo-workflow",
		TaskQueue: taskQueue,
	}, retry_and_errors.RetryDemoWorkflow)
	if err != nil {
		log.Fatal("Unable to start workflow:", err)
	}

	var result string
	if err := we.Get(context.Background(), &result); err != nil {
		log.Fatal("Workflow failed:", err)
	}

	fmt.Println("Result:", result)
}
