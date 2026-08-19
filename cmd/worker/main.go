package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"taskboard/internal/configenv"
	"taskboard/internal/store"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	databaseURLDefault, err := configenv.String("DATABASE_URL", "postgres://taskboard:taskboard@localhost:54329/taskboard?sslmode=disable")
	if err != nil {
		logger.Error("invalid database configuration", "error", err)
		os.Exit(1)
	}
	workerIDDefault, err := configenv.String("WORKER_ID", defaultWorkerID())
	if err != nil {
		logger.Error("invalid worker configuration", "error", err)
		os.Exit(1)
	}
	stepDelayDefault := requiredDuration(logger, "STEP_DELAY", 15*time.Second)
	pollIntervalDefault := requiredDuration(logger, "POLL_INTERVAL", time.Second)
	leaseDurationDefault := requiredDuration(logger, "LEASE_DURATION", 30*time.Second)
	heartbeatIntervalDefault := requiredDuration(logger, "HEARTBEAT_INTERVAL", 10*time.Second)

	databaseURL := flag.String("database-url", databaseURLDefault, "PostgreSQL connection URL")
	workerID := flag.String("id", workerIDDefault, "unique worker ID")
	stepDelay := flag.Duration("step-delay", stepDelayDefault, "simulated execution time per step")
	pollInterval := flag.Duration("poll-interval", pollIntervalDefault, "delay when no task is pending")
	leaseDuration := flag.Duration("lease-duration", leaseDurationDefault, "task ownership lease duration")
	heartbeatInterval := flag.Duration("heartbeat-interval", heartbeatIntervalDefault, "lease renewal interval")
	flag.Parse()
	if *stepDelay < 0 || *pollInterval <= 0 || *leaseDuration <= 0 ||
		*heartbeatInterval <= 0 || *heartbeatInterval >= *leaseDuration {
		fmt.Fprintln(os.Stderr, "delays must be non-negative, intervals positive, and heartbeat shorter than lease")
		os.Exit(2)
	}

	logger = logger.With("worker", *workerID)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dataStore, err := store.Open(ctx, *databaseURL)
	if err != nil {
		logger.Error("database connection failed", "error", err)
		os.Exit(1)
	}
	defer dataStore.Close()
	if err := dataStore.Migrate(ctx); err != nil {
		logger.Error("database migration failed", "error", err)
		os.Exit(1)
	}

	logger.Info("worker started", "step_delay", *stepDelay, "poll_interval", *pollInterval,
		"lease_duration", *leaseDuration, "heartbeat_interval", *heartbeatInterval)
	for ctx.Err() == nil {
		claimed, err := dataStore.ClaimNext(ctx, *workerID, *leaseDuration)
		if err != nil {
			logger.Error("claim failed", "error", err)
			wait(ctx, *pollInterval)
			continue
		}
		if claimed == nil {
			wait(ctx, *pollInterval)
			continue
		}
		logger.Info("task claimed", "task_id", claimed.ID, "fencing_token", claimed.FencingToken,
			"recovered", claimed.Recovered, "lease_expires_at", claimed.LeaseExpiresAt)
		if err := executeTask(
			ctx, dataStore, logger, claimed, *stepDelay, *leaseDuration, *heartbeatInterval,
		); err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("task execution stopped", "task_id", claimed.ID, "error", err)
		}
	}
}

func executeTask(
	ctx context.Context,
	dataStore *store.Store,
	logger *slog.Logger,
	claimed *store.ClaimedTask,
	delay time.Duration,
	leaseDuration time.Duration,
	heartbeatInterval time.Duration,
) error {
	for ctx.Err() == nil {
		step, err := dataStore.StartCurrentStep(ctx, claimed.ID, claimed.WorkerID, claimed.FencingToken)
		if errors.Is(err, store.ErrTaskTerminal) {
			return nil
		}
		if err != nil {
			return err
		}
		logger.Info("step running", "task_id", claimed.ID, "step", step.Position,
			"idempotency_key", step.IdempotencyKey, "parameters", step.ResolvedParameters)
		if err := waitWithHeartbeat(
			ctx, dataStore, logger, claimed, delay, leaseDuration, heartbeatInterval,
		); err != nil {
			return err
		}
		result, err := dataStore.CompleteStep(
			ctx, claimed.ID, step.Position, claimed.WorkerID, claimed.FencingToken, true,
		)
		if err != nil {
			return err
		}
		logger.Info("step reported", "task_id", claimed.ID, "step", step.Position, "task_status", result.TaskStatus)
		if result.TaskStatus == "done" {
			return nil
		}
	}
	return ctx.Err()
}

func waitWithHeartbeat(
	ctx context.Context,
	dataStore *store.Store,
	logger *slog.Logger,
	claimed *store.ClaimedTask,
	delay time.Duration,
	leaseDuration time.Duration,
	heartbeatInterval time.Duration,
) error {
	done := time.NewTimer(delay)
	defer done.Stop()
	heartbeats := time.NewTicker(heartbeatInterval)
	defer heartbeats.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-done.C:
			return nil
		case <-heartbeats.C:
			expiresAt, err := dataStore.RenewLease(
				ctx, claimed.ID, claimed.WorkerID, claimed.FencingToken, leaseDuration,
			)
			if err != nil {
				return err
			}
			logger.Debug("task lease renewed", "task_id", claimed.ID, "lease_expires_at", expiresAt)
		}
	}
}

func wait(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func requiredDuration(logger *slog.Logger, key string, fallback time.Duration) time.Duration {
	parsed, err := configenv.Duration(key, fallback)
	if err != nil {
		logger.Error("invalid worker configuration", "error", err)
		os.Exit(1)
	}
	return parsed
}

func defaultWorkerID() string {
	hostname, _ := os.Hostname()
	return fmt.Sprintf("%s-%d", hostname, os.Getpid())
}
