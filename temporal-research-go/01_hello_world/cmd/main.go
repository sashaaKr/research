package main

import (
	"context"
	"fmt"
	"log"

	hello_world "temporal-go/01_hello_world"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

const taskQueue = "hello-world-queue"

func main() {
	c, err := client.Dial(client.Options{})
	if err != nil {
		log.Fatal("Unable to create Temporal client:", err)
	}
	defer c.Close()

	w := worker.New(c, taskQueue, worker.Options{})
	w.RegisterWorkflow(hello_world.HelloWorkflow)
	w.RegisterActivity(hello_world.SayHello)

	go func() {
		if err := w.Run(worker.InterruptCh()); err != nil {
			log.Fatal("Worker error:", err)
		}
	}()

	we, err := c.ExecuteWorkflow(context.Background(), client.StartWorkflowOptions{
		ID:        "hello-world-workflow",
		TaskQueue: taskQueue,
	}, hello_world.HelloWorkflow, "World")
	if err != nil {
		log.Fatal("Unable to start workflow:", err)
	}

	var result string
	if err := we.Get(context.Background(), &result); err != nil {
		log.Fatal("Workflow failed:", err)
	}

	fmt.Println(result)
}
