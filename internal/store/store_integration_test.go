package store_test

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"taskboard/internal/parameters"
	"taskboard/internal/store"
)

func TestStickyParametersAndIdempotentCompletion(t *testing.T) {
	ctx := context.Background()
	dataStore, databaseURL := integrationStore(t, ctx)
	defer dataStore.Close()

	taskID, err := dataStore.CreateTask(ctx, store.CreateTaskInput{
		GroupName:      "integration",
		GroupOverrides: parameters.Values{"source": "group", "literal_empty": "", "new_group": true},
		BaseParameters: parameters.Values{"source": "base", "region": "cn", "retries": float64(1)},
		Steps: []store.StepInput{
			{Overrides: parameters.Values{"source": "step-1", "region": ""}},
			{Overrides: parameters.Values{"source": "", "retries": float64(2)}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := dataStore.ClaimNext(ctx, "integration-worker", time.Minute)
	if err != nil || claimed == nil || claimed.ID != taskID {
		t.Fatalf("claim: task=%+v err=%v", claimed, err)
	}
	first, err := dataStore.StartCurrentStep(ctx, taskID, "integration-worker", claimed.FencingToken)
	if err != nil {
		t.Fatal(err)
	}
	if first.ResolvedParameters["source"] != "step-1" || first.ResolvedParameters["region"] != "cn" {
		t.Fatalf("unexpected first-step parameters: %#v", first.ResolvedParameters)
	}
	if first.ResolvedParameters["literal_empty"] != "" {
		t.Fatalf("group empty string must remain literal: %#v", first.ResolvedParameters)
	}

	const reports = 20
	var waitGroup sync.WaitGroup
	errorsByReport := make(chan error, reports)
	for index := 0; index < reports; index++ {
		waitGroup.Add(1)
		go func(success bool) {
			defer waitGroup.Done()
			_, err := dataStore.CompleteStep(ctx, taskID, 1, "integration-worker", claimed.FencingToken, success)
			errorsByReport <- err
		}(index%3 == 0)
	}
	waitGroup.Wait()
	close(errorsByReport)
	for err := range errorsByReport {
		if err != nil {
			t.Fatalf("concurrent report failed: %v", err)
		}
	}

	databasePool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer databasePool.Close()
	var logCount int
	var finalSuccess bool
	if err := databasePool.QueryRow(ctx,
		`SELECT count(*), bool_and(success) FROM step_logs WHERE task_id = $1 AND step_position = 1`, taskID,
	).Scan(&logCount, &finalSuccess); err != nil {
		t.Fatal(err)
	}
	if logCount != 1 || !finalSuccess {
		t.Fatalf("want one successful log, got count=%d success=%v", logCount, finalSuccess)
	}
	var reportEvents int
	if err := databasePool.QueryRow(ctx,
		`SELECT count(*) FROM activity_logs WHERE task_id = $1 AND event_type = 'step_reported'`, taskID,
	).Scan(&reportEvents); err != nil {
		t.Fatal(err)
	}
	if reportEvents != reports {
		t.Fatalf("want %d received-report activity entries, got %d", reports, reportEvents)
	}

	second, err := dataStore.StartCurrentStep(ctx, taskID, "integration-worker", claimed.FencingToken)
	if err != nil {
		t.Fatal(err)
	}
	if second.Position != 2 || second.ResolvedParameters["source"] != "step-1" || second.ResolvedParameters["retries"] != float64(2) {
		t.Fatalf("sticky parameters did not carry forward: %#v", second)
	}
	if _, err := dataStore.CompleteStep(ctx, taskID, 2, "integration-worker", claimed.FencingToken, true); err != nil {
		t.Fatal(err)
	}
	page, err := dataStore.ListTasks(ctx, 25, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Tasks) != 1 || page.Tasks[0].Status != "done" {
		t.Fatalf("expected completed task, got %#v", page.Tasks)
	}
}

func TestFailureCanUpgradeButSuccessCannotDowngrade(t *testing.T) {
	ctx := context.Background()
	dataStore, databaseURL := integrationStore(t, ctx)
	defer dataStore.Close()

	taskID, err := dataStore.CreateTask(ctx, store.CreateTaskInput{
		GroupName: "result-priority",
		Steps:     []store.StepInput{{Overrides: parameters.Values{}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := dataStore.ClaimNext(ctx, "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dataStore.StartCurrentStep(ctx, taskID, "worker", claimed.FencingToken); err != nil {
		t.Fatal(err)
	}
	failed, err := dataStore.CompleteStep(ctx, taskID, 1, "worker", claimed.FencingToken, false)
	if err != nil || failed.TaskStatus != "failed" || failed.Success || failed.DuplicateReport || failed.LogCount != 1 {
		t.Fatalf("expected initial failure, result=%+v err=%v", failed, err)
	}
	upgraded, err := dataStore.CompleteStep(ctx, taskID, 1, "worker", claimed.FencingToken, true)
	if err != nil || upgraded.TaskStatus != "done" || !upgraded.Success || !upgraded.DuplicateReport || upgraded.LogCount != 1 {
		t.Fatalf("expected success upgrade, result=%+v err=%v", upgraded, err)
	}
	notDowngraded, err := dataStore.CompleteStep(ctx, taskID, 1, "worker", claimed.FencingToken, false)
	if err != nil || !notDowngraded.Success || notDowngraded.TaskStatus != "done" {
		t.Fatalf("success was downgraded, result=%+v err=%v", notDowngraded, err)
	}

	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var count int
	var success bool
	if err := pool.QueryRow(ctx,
		`SELECT count(*), bool_and(success) FROM step_logs WHERE task_id = $1`, taskID,
	).Scan(&count, &success); err != nil {
		t.Fatal(err)
	}
	if count != 1 || !success {
		t.Fatalf("unexpected final log: count=%d success=%v", count, success)
	}
}

func TestExpiredLeaseIsRecoveredAndOldWorkerIsFenced(t *testing.T) {
	ctx := context.Background()
	dataStore, databaseURL := integrationStore(t, ctx)
	defer dataStore.Close()

	taskID, err := dataStore.CreateTask(ctx, store.CreateTaskInput{
		GroupName:      "lease-recovery",
		GroupOverrides: parameters.Values{"campaign": "original"},
		Steps:          []store.StepInput{{Overrides: parameters.Values{"attempt": "sticky"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	oldOwner, err := dataStore.ClaimNext(ctx, "worker-old", 80*time.Millisecond)
	if err != nil || oldOwner == nil {
		t.Fatalf("old owner claim: task=%+v err=%v", oldOwner, err)
	}
	started, err := dataStore.StartCurrentStep(ctx, taskID, oldOwner.WorkerID, oldOwner.FencingToken)
	if err != nil || started.Position != 1 {
		t.Fatalf("start with old owner: step=%+v err=%v", started, err)
	}
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `
		UPDATE task_groups SET overrides = '{"campaign":"changed-after-start"}'::jsonb
		WHERE id = (SELECT group_id FROM tasks WHERE id = $1)
	`, taskID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(120 * time.Millisecond)

	newOwner, err := dataStore.ClaimNext(ctx, "worker-new", time.Minute)
	if err != nil || newOwner == nil || !newOwner.Recovered || newOwner.ID != taskID {
		t.Fatalf("recovery claim: task=%+v err=%v", newOwner, err)
	}
	if newOwner.FencingToken <= oldOwner.FencingToken {
		t.Fatalf("fencing token did not advance: old=%d new=%d", oldOwner.FencingToken, newOwner.FencingToken)
	}
	if newOwner.EffectiveParameters["campaign"] != "original" {
		t.Fatalf("recovery re-read changed group parameters: %#v", newOwner.EffectiveParameters)
	}
	if _, err := dataStore.RenewLease(ctx, taskID, oldOwner.WorkerID, oldOwner.FencingToken, time.Minute); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("old owner renewed recovered lease: %v", err)
	}
	if _, err := dataStore.CompleteStep(ctx, taskID, 1, oldOwner.WorkerID, oldOwner.FencingToken, true); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("old owner completed recovered task: %v", err)
	}
	restarted, err := dataStore.StartCurrentStep(ctx, taskID, newOwner.WorkerID, newOwner.FencingToken)
	if err != nil || restarted.ResolvedParameters["attempt"] != "sticky" ||
		restarted.IdempotencyKey != started.IdempotencyKey {
		t.Fatalf("new owner did not resume current step: step=%+v err=%v", restarted, err)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id = $1`, taskID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "running" {
		t.Fatalf("resumed task status = %q, want running", status)
	}
	if _, err := dataStore.CompleteStep(ctx, taskID, 1, newOwner.WorkerID, newOwner.FencingToken, true); err != nil {
		t.Fatal(err)
	}
	logs, err := dataStore.ListActivity(ctx, 20)
	if err != nil {
		t.Fatal(err)
	}
	foundReclaim := false
	for _, log := range logs {
		foundReclaim = foundReclaim || log.EventType == "task_reclaimed"
	}
	if !foundReclaim {
		t.Fatalf("recovery event missing: %#v", logs)
	}
}

func TestTaskPaginationRetentionAndDemoPruning(t *testing.T) {
	ctx := context.Background()
	dataStore, databaseURL := integrationStore(t, ctx)
	defer dataStore.Close()

	for index := 0; index < 30; index++ {
		if _, err := dataStore.CreateTask(ctx, store.CreateTaskInput{
			GroupName: "page", Steps: []store.StepInput{{Overrides: parameters.Values{}}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	page, err := dataStore.ListTasks(ctx, 10, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Tasks) != 10 || page.Tasks[0].ID != 20 || page.Tasks[9].ID != 11 ||
		page.Summary.Total != 30 || page.Summary.Pending != 30 {
		t.Fatalf("unexpected task page: %#v", page)
	}

	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `
		UPDATE tasks SET status = 'done', updated_at = now() - interval '48 hours' WHERE id IN (1, 2);
		UPDATE tasks SET updated_at = now() - interval '48 hours' WHERE id = 3;
	`); err != nil {
		t.Fatal(err)
	}
	deleted, err := dataStore.PruneTerminalTasks(ctx, time.Now().Add(-24*time.Hour), 100)
	if err != nil || deleted != 2 {
		t.Fatalf("retention deleted=%d err=%v", deleted, err)
	}
	page, err = dataStore.ListTasks(ctx, 200, 0)
	if err != nil || page.Summary.Total != 28 || page.Summary.Pending != 28 {
		t.Fatalf("retention summary=%+v err=%v", page, err)
	}

	for range 12 {
		taskID, err := dataStore.CreateClaimDemoTask(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := dataStore.FinishClaimDemo(ctx, taskID); err != nil {
			t.Fatal(err)
		}
	}
	var demos, orphanGroups int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE demo_claim`).Scan(&demos); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM task_groups g
		WHERE NOT EXISTS (SELECT 1 FROM tasks t WHERE t.group_id = g.id)
	`).Scan(&orphanGroups); err != nil {
		t.Fatal(err)
	}
	if demos != 10 || orphanGroups != 0 {
		t.Fatalf("demo retention left demos=%d orphan_groups=%d", demos, orphanGroups)
	}
}

func TestCancellationIsTerminalAndFencesRunningWorker(t *testing.T) {
	ctx := context.Background()
	dataStore, _ := integrationStore(t, ctx)
	defer dataStore.Close()

	taskID, err := dataStore.CreateTask(ctx, store.CreateTaskInput{
		GroupName: "cancellation",
		Steps: []store.StepInput{
			{Overrides: parameters.Values{}},
			{Overrides: parameters.Values{}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := dataStore.ClaimNext(ctx, "cancelled-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dataStore.StartCurrentStep(ctx, taskID, claimed.WorkerID, claimed.FencingToken); err != nil {
		t.Fatal(err)
	}
	changed, err := dataStore.CancelTask(ctx, taskID, "admin")
	if err != nil || !changed {
		t.Fatalf("cancel: changed=%v err=%v", changed, err)
	}
	changed, err = dataStore.CancelTask(ctx, taskID, "admin")
	if err != nil || changed {
		t.Fatalf("idempotent cancel: changed=%v err=%v", changed, err)
	}
	if _, err := dataStore.CompleteStep(ctx, taskID, 1, claimed.WorkerID, claimed.FencingToken, true); !errors.Is(err, store.ErrTaskTerminal) {
		t.Fatalf("cancelled worker completed task: %v", err)
	}
	page, err := dataStore.ListTasks(ctx, 25, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Tasks) != 1 || page.Tasks[0].Status != "cancelled" ||
		page.Tasks[0].Steps[0].Status != "cancelled" || page.Tasks[0].Steps[1].Status != "cancelled" {
		t.Fatalf("unexpected cancelled task: %#v", page.Tasks)
	}
}

func integrationStore(t *testing.T, ctx context.Context) (*store.Store, string) {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	dataStore, err := store.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if err := dataStore.Migrate(ctx); err != nil {
		dataStore.Close()
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		dataStore.Close()
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE activity_logs, step_logs, steps, tasks, task_groups RESTART IDENTITY CASCADE`); err != nil {
		pool.Close()
		dataStore.Close()
		t.Fatal(err)
	}
	pool.Close()
	return dataStore, databaseURL
}
