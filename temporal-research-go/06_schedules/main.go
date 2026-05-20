package main

import (
	"context"
	"fmt"
	"log"
	"time"

	schedules "temporal-go/06_schedules"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

const taskQueue = "schedule-queue"

func main() {
	c, err := client.Dial(client.Options{})
	if err != nil {
		log.Fatal("Unable to create Temporal client:", err)
	}
	defer c.Close()

	w := worker.New(c, taskQueue, worker.Options{})
	w.RegisterWorkflow(schedules.ReportWorkflow)
	w.RegisterActivity(schedules.GenerateReport)

	go func() {
		if err := w.Run(worker.InterruptCh()); err != nil {
			log.Fatal("Worker error:", err)
		}
	}()

	ctx := context.Background()

	scheduleID := "daily-report"

	// 1. Create the schedule
	scheduleClient := c.ScheduleClient()
	handle, err := scheduleClient.Create(ctx, client.ScheduleOptions{
		ID: scheduleID,
		Spec: client.ScheduleSpec{
			CronExpressions: []string{"0 9 * * *"},
		},
		Action: &client.ScheduleWorkflowAction{
			ID:        "daily-report-workflow",
			Workflow:  schedules.ReportWorkflow,
			TaskQueue: taskQueue,
			Args:      []interface{}{"daily"},
		},
	})
	if err != nil {
		log.Printf("Schedule creation warning (may already exist): %v", err)
	} else {
		fmt.Println("Schedule created:", handle.GetID())
	}

	// 2. List existing schedules
	listHandle := scheduleClient.List(ctx, client.ScheduleListOptions{})
	fmt.Println("Existing schedules:")
	for listHandle.HasNext() {
		entry, err := listHandle.Next()
		if err != nil {
			log.Printf("Error listing schedules: %v", err)
			break
		}
		fmt.Printf("  - %s\n", entry.ID)
	}

	// 3. Pause the schedule
	if handle != nil {
		if err := handle.Pause(ctx, client.SchedulePauseOptions{Note: "pausing for demo"}); err != nil {
			log.Printf("Failed to pause schedule: %v", err)
		} else {
			fmt.Println("Schedule paused")
		}

		// 4. Delete the schedule
		if err := handle.Delete(ctx); err != nil {
			log.Printf("Failed to delete schedule: %v", err)
		} else {
			fmt.Println("Schedule deleted")
		}
	}

	// 5. Run ReportWorkflow directly (standalone)
	time.Sleep(500 * time.Millisecond)
	we, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        "manual-report-workflow",
		TaskQueue: taskQueue,
	}, schedules.ReportWorkflow, "manual")
	if err != nil {
		log.Fatal("Unable to start workflow:", err)
	}

	var result string
	if err := we.Get(ctx, &result); err != nil {
		log.Fatal("Workflow failed:", err)
	}

	fmt.Println("Manual report result:", result)
}
