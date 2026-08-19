package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"taskboard/internal/configenv"
	"taskboard/internal/httpapi"
	"taskboard/internal/store"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	databaseURLDefault, err := configenv.String("DATABASE_URL", "postgres://taskboard:taskboard@localhost:54329/taskboard?sslmode=disable")
	if err != nil {
		logger.Error("invalid database configuration", "error", err)
		os.Exit(1)
	}
	address := flag.String("address", envOr("ADDRESS", ":8080"), "HTTP listen address")
	databaseURL := flag.String("database-url", databaseURLDefault, "PostgreSQL connection URL")
	migrateOnly := flag.Bool("migrate-only", false, "apply migrations and exit")
	flag.Parse()

	retention, err := configenv.Duration("TASK_RETENTION", 0)
	if err != nil || retention < 0 {
		logger.Error("invalid task retention configuration", "value", retention, "error", err)
		os.Exit(1)
	}
	retentionInterval, err := configenv.Duration("RETENTION_INTERVAL", 10*time.Minute)
	if err != nil || retentionInterval <= 0 {
		logger.Error("invalid retention interval configuration", "value", retentionInterval, "error", err)
		os.Exit(1)
	}

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
	if *migrateOnly {
		logger.Info("database migrations applied")
		return
	}
	server := &http.Server{
		Addr:              *address,
		Handler:           httpapi.NewHandler(dataStore, logger),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	if retention > 0 {
		go runTaskRetention(ctx, dataStore, logger, retention, retentionInterval)
	}

	logger.Info("taskboard server started", "address", *address, "task_retention", retention)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("server stopped unexpectedly", "error", err)
		os.Exit(1)
	}
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func runTaskRetention(
	ctx context.Context,
	dataStore *store.Store,
	logger *slog.Logger,
	retention time.Duration,
	interval time.Duration,
) {
	prune := func() {
		var total int64
		for range 10 {
			deleted, err := dataStore.PruneTerminalTasks(ctx, time.Now().Add(-retention), 1000)
			if err != nil {
				if ctx.Err() == nil {
					logger.Error("task retention failed", "error", err)
				}
				return
			}
			total += deleted
			if deleted < 1000 {
				break
			}
		}
		if total > 0 {
			logger.Info("expired terminal tasks pruned", "count", total, "retention", retention)
		}
	}
	prune()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			prune()
		}
	}
}
