package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"time"

	template_runner "temporal-go/08_template_runner"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

const taskQueue = "template-runner-queue"

// ANSI colour/style helpers
const (
	ansiReset  = "\033[0m"
	ansiGreen  = "\033[32m"
	ansiYellow = "\033[33m"
	ansiRed    = "\033[31m"
	ansiGrey   = "\033[90m"
	ansiDim    = "\033[2m"
	ansiClear  = "\033[2J\033[H"
)

// renderGraph prints the current execution graph grouped by status.
func renderGraph(graph template_runner.ExecutionGraph) {
	fmt.Print(ansiClear)
	fmt.Printf("=== Execution Graph: %s ===\n\n", graph.TemplateID)

	// Group nodes by status for a tidy display.
	groups := map[template_runner.NodeStatus][]template_runner.NodeState{
		template_runner.Running:   {},
		template_runner.Completed: {},
		template_runner.Skipped:   {},
		template_runner.Pending:   {},
		template_runner.Failed:    {},
	}
	for _, ns := range graph.Nodes {
		groups[ns.Status] = append(groups[ns.Status], ns)
	}

	printGroup := func(status template_runner.NodeStatus, icon, colour string) {
		nodes := groups[status]
		if len(nodes) == 0 {
			return
		}
		sort.Slice(nodes, func(i, j int) bool { return nodes[i].NodeID < nodes[j].NodeID })
		fmt.Printf("%s%s %s (%d)%s\n", colour, icon, string(status), len(nodes), ansiReset)
		for _, ns := range nodes {
			switch status {
			case template_runner.Completed:
				fmt.Printf("  %s✓ %s%s\n", ansiGreen, ns.NodeName, ansiReset)
			case template_runner.Running:
				fmt.Printf("  %s◉ %s%s\n", ansiYellow, ns.NodeName, ansiReset)
			case template_runner.Skipped:
				fmt.Printf("  %s⊘ %s%s\n", ansiGrey, ns.NodeName, ansiReset)
			case template_runner.Pending:
				fmt.Printf("  %s· %s%s\n", ansiDim, ns.NodeName, ansiReset)
			case template_runner.Failed:
				fmt.Printf("  %s✗ %s: %s%s\n", ansiRed, ns.NodeName, ns.Error, ansiReset)
			}
		}
		fmt.Println()
	}

	printGroup(template_runner.Running, "◉", ansiYellow)
	printGroup(template_runner.Completed, "✓", ansiGreen)
	printGroup(template_runner.Skipped, "⊘", ansiGrey)
	printGroup(template_runner.Pending, "·", ansiDim)
	printGroup(template_runner.Failed, "✗", ansiRed)
}

// allTerminal returns true when every node in the graph has a terminal status.
func allTerminal(graph template_runner.ExecutionGraph) bool {
	for _, ns := range graph.Nodes {
		if ns.Status == template_runner.Pending || ns.Status == template_runner.Running {
			return false
		}
	}
	return true
}

func main() {
	c, err := client.Dial(client.Options{})
	if err != nil {
		log.Fatal("Unable to create Temporal client:", err)
	}
	defer c.Close()

	w := worker.New(c, taskQueue, worker.Options{})

	// Register the interpreter workflow.
	w.RegisterWorkflow(template_runner.TemplateRunnerWorkflow)

	// Register activities by name so ExecuteActivity can resolve them by string.
	w.RegisterActivityWithOptions(template_runner.ProvisionVPC, activity.RegisterOptions{Name: "ProvisionVPC"})
	w.RegisterActivityWithOptions(template_runner.ProvisionSubnet, activity.RegisterOptions{Name: "ProvisionSubnet"})
	w.RegisterActivityWithOptions(template_runner.ProvisionServer, activity.RegisterOptions{Name: "ProvisionServer"})
	w.RegisterActivityWithOptions(template_runner.ProvisionDatabase, activity.RegisterOptions{Name: "ProvisionDatabase"})
	w.RegisterActivityWithOptions(template_runner.ProvisionCache, activity.RegisterOptions{Name: "ProvisionCache"})
	w.RegisterActivityWithOptions(template_runner.ProvisionLB, activity.RegisterOptions{Name: "ProvisionLB"})
	w.RegisterActivityWithOptions(template_runner.RegisterDNS, activity.RegisterOptions{Name: "RegisterDNS"})
	w.RegisterActivityWithOptions(template_runner.RunHealthCheck, activity.RegisterOptions{Name: "RunHealthCheck"})

	go func() {
		if err := w.Run(worker.InterruptCh()); err != nil {
			log.Fatal("Worker error:", err)
		}
	}()

	// Submit the workflow.
	we, err := c.ExecuteWorkflow(context.Background(), client.StartWorkflowOptions{
		ID:        fmt.Sprintf("template-runner-%d", time.Now().Unix()),
		TaskQueue: taskQueue,
	}, template_runner.TemplateRunnerWorkflow, template_runner.ProvisioningTemplate, template_runner.StagingContext)
	if err != nil {
		log.Fatal("Unable to start workflow:", err)
	}

	wfID := we.GetID()
	wfRunID := we.GetRunID()
	fmt.Printf("Started workflow: %s / %s\n\n", wfID, wfRunID)

	// queryGraph queries the workflow for the current execution graph.
	queryGraph := func() (template_runner.ExecutionGraph, error) {
		resp, err := c.QueryWorkflow(context.Background(), wfID, wfRunID, "get_execution_graph")
		if err != nil {
			return template_runner.ExecutionGraph{}, err
		}
		var graph template_runner.ExecutionGraph
		if err := resp.Get(&graph); err != nil {
			return template_runner.ExecutionGraph{}, err
		}
		return graph, nil
	}

	// Poll the query handler every 500ms and render the graph until the workflow finishes.
	done := make(chan struct{})
	var workflowResult map[string]any
	var workflowErr error

	go func() {
		defer close(done)
		workflowErr = we.Get(context.Background(), &workflowResult)
	}()

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

polling:
	for {
		select {
		case <-ticker.C:
			if graph, err := queryGraph(); err == nil {
				renderGraph(graph)
				if allTerminal(graph) {
					break polling
				}
			}
		case <-done:
			break polling
		}
	}

	// Wait for the goroutine to finish if it hasn't yet.
	<-done

	if workflowErr != nil {
		log.Fatal("Workflow failed:", workflowErr)
	}

	// Final graph render.
	if finalGraph, err := queryGraph(); err == nil {
		renderGraph(finalGraph)
	}

	// Print the workflow output.
	out, err := json.MarshalIndent(workflowResult, "", "  ")
	if err != nil {
		log.Fatal("Failed to marshal result:", err)
	}
	fmt.Println("\n=== Workflow Output ===")
	fmt.Println(string(out))
}
