package main

import (
	"context"
	"fmt"
	"log"

	child_workflows "temporal-go/05_child_workflows"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

const taskQueue = "batch-queue"

func main() {
	c, err := client.Dial(client.Options{})
	if err != nil {
		log.Fatal("Unable to create Temporal client:", err)
	}
	defer c.Close()

	w := worker.New(c, taskQueue, worker.Options{})
	w.RegisterWorkflow(child_workflows.BatchWorkflow)
	w.RegisterWorkflow(child_workflows.ItemWorkflow)
	w.RegisterActivity(child_workflows.ProcessItem)

	go func() {
		if err := w.Run(worker.InterruptCh()); err != nil {
			log.Fatal("Worker error:", err)
		}
	}()

	items := []string{"item-1", "item-2", "item-3", "item-4", "item-5"}

	we, err := c.ExecuteWorkflow(context.Background(), client.StartWorkflowOptions{
		ID:        "batch-workflow",
		TaskQueue: taskQueue,
	}, child_workflows.BatchWorkflow, items)
	if err != nil {
		log.Fatal("Unable to start workflow:", err)
	}

	var results []string
	if err := we.Get(context.Background(), &results); err != nil {
		log.Fatal("Workflow failed:", err)
	}

	fmt.Println("Batch results:")
	for _, r := range results {
		fmt.Println(" -", r)
	}
}
