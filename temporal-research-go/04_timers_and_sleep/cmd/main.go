package main

import (
	"context"
	"fmt"
	"log"

	timers_and_sleep "temporal-go/04_timers_and_sleep"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

const taskQueue = "timer-queue"

func main() {
	c, err := client.Dial(client.Options{})
	if err != nil {
		log.Fatal("Unable to create Temporal client:", err)
	}
	defer c.Close()

	w := worker.New(c, taskQueue, worker.Options{})
	w.RegisterWorkflow(timers_and_sleep.CheckInWorkflow)
	w.RegisterActivity(timers_and_sleep.CheckStatus)

	go func() {
		if err := w.Run(worker.InterruptCh()); err != nil {
			log.Fatal("Worker error:", err)
		}
	}()

	we, err := c.ExecuteWorkflow(context.Background(), client.StartWorkflowOptions{
		ID:        "checkin-workflow",
		TaskQueue: taskQueue,
	}, timers_and_sleep.CheckInWorkflow, "item-42")
	if err != nil {
		log.Fatal("Unable to start workflow:", err)
	}

	var result string
	if err := we.Get(context.Background(), &result); err != nil {
		log.Fatal("Workflow failed:", err)
	}

	fmt.Println("Result:", result)
}
