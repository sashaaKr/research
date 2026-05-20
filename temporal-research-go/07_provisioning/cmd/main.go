package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	provisioning "temporal-go/07_provisioning"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

const taskQueue = "provisioning-queue"

func main() {
	c, err := client.Dial(client.Options{})
	if err != nil {
		log.Fatal("Unable to create Temporal client:", err)
	}
	defer c.Close()

	w := worker.New(c, taskQueue, worker.Options{})

	// Register workflows
	w.RegisterWorkflow(provisioning.EnvironmentWorkflow)
	w.RegisterWorkflow(provisioning.ServerWorkflow)

	// Register activities
	w.RegisterActivity(provisioning.ProvisionVPC)
	w.RegisterActivity(provisioning.ProvisionSubnet)
	w.RegisterActivity(provisioning.ProvisionServer)
	w.RegisterActivity(provisioning.ProvisionDatabase)
	w.RegisterActivity(provisioning.ProvisionCache)
	w.RegisterActivity(provisioning.ProvisionLoadBalancer)
	w.RegisterActivity(provisioning.RegisterDNS)
	w.RegisterActivity(provisioning.RunHealthCheck)

	go func() {
		if err := w.Run(worker.InterruptCh()); err != nil {
			log.Fatal("Worker error:", err)
		}
	}()

	req := provisioning.ProvisionRequest{
		Env:          "staging",
		Region:       "us-east-1",
		ServerCount:  3,
		IncludeDB:    true,
		IncludeCache: true,
		IncludeLB:    true,
	}

	we, err := c.ExecuteWorkflow(context.Background(), client.StartWorkflowOptions{
		ID:        "environment-workflow-staging",
		TaskQueue: taskQueue,
	}, provisioning.EnvironmentWorkflow, req)
	if err != nil {
		log.Fatal("Unable to start workflow:", err)
	}

	var manifest provisioning.EnvManifest
	if err := we.Get(context.Background(), &manifest); err != nil {
		log.Fatal("Workflow failed:", err)
	}

	out, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		log.Fatal("Failed to marshal manifest:", err)
	}
	fmt.Println(string(out))
}
