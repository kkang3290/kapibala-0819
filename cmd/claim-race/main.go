package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"taskboard/internal/configenv"
	"taskboard/internal/parameters"
	"taskboard/internal/store"
)

type options struct {
	databaseURL string
	workers     int
	tasks       int
	child       bool
	startAt     int64
}

func main() {
	var options options
	databaseURLDefault, err := configenv.String("DATABASE_URL", "postgres://taskboard:taskboard@localhost:54329/taskboard?sslmode=disable")
	if err != nil {
		fmt.Fprintln(os.Stderr, "claim race failed:", err)
		os.Exit(1)
	}
	flag.StringVar(&options.databaseURL, "database-url", databaseURLDefault, "PostgreSQL connection URL")
	flag.IntVar(&options.workers, "workers", 10, "number of OS worker processes")
	flag.IntVar(&options.tasks, "tasks", 250, "number of tasks to claim")
	flag.BoolVar(&options.child, "child", false, "run as a claim worker")
	flag.Int64Var(&options.startAt, "start-at", 0, "coordinated start time in Unix nanoseconds")
	flag.Parse()

	ctx := context.Background()
	var runErr error
	if options.child {
		runErr = runChild(ctx, options)
	} else {
		runErr = runCoordinator(ctx, options)
	}
	if runErr != nil {
		fmt.Fprintln(os.Stderr, "claim race failed:", runErr)
		os.Exit(1)
	}
}

func runCoordinator(ctx context.Context, options options) error {
	if options.workers < 2 || options.tasks < 1 {
		return errors.New("workers must be at least 2 and tasks must be positive")
	}
	runName := fmt.Sprintf("taskboard_race_%d_%d", os.Getpid(), time.Now().UnixNano())
	adminURL, err := replaceDatabase(options.databaseURL, "postgres")
	if err != nil {
		return err
	}
	runURL, err := replaceDatabase(options.databaseURL, runName)
	if err != nil {
		return err
	}
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		return fmt.Errorf("connect to maintenance database: %w", err)
	}
	defer admin.Close(ctx)
	identifier := pgx.Identifier{runName}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+identifier); err != nil {
		return fmt.Errorf("create isolated race database: %w", err)
	}
	defer func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE "+identifier+" WITH (FORCE)")
	}()

	raceStore, err := store.Open(ctx, runURL)
	if err != nil {
		return err
	}
	if err := raceStore.Migrate(ctx); err != nil {
		raceStore.Close()
		return err
	}
	for index := 0; index < options.tasks; index++ {
		_, err := raceStore.CreateTask(ctx, store.CreateTaskInput{
			GroupName:      fmt.Sprintf("race-%d", index),
			GroupOverrides: parameters.Values{},
			BaseParameters: parameters.Values{"run": runName},
			Steps:          []store.StepInput{{Overrides: parameters.Values{}}},
		})
		if err != nil {
			raceStore.Close()
			return fmt.Errorf("seed race task: %w", err)
		}
	}
	raceStore.Close()

	executable, err := os.Executable()
	if err != nil {
		return err
	}
	startAt := time.Now().Add(1200 * time.Millisecond).UnixNano()
	type process struct {
		command *exec.Cmd
		stdout  bytes.Buffer
		stderr  bytes.Buffer
	}
	processes := make([]process, options.workers)
	for index := range processes {
		command := exec.Command(executable,
			"-child",
			"-database-url", runURL,
			"-start-at", strconv.FormatInt(startAt, 10),
		)
		processes[index].command = command
		command.Stdout = &processes[index].stdout
		command.Stderr = &processes[index].stderr
		if err := command.Start(); err != nil {
			return fmt.Errorf("start worker process %d: %w", index, err)
		}
	}

	claims := make(map[int64]int)
	for index := range processes {
		if err := processes[index].command.Wait(); err != nil {
			return fmt.Errorf("worker process %d: %w: %s", index, err, processes[index].stderr.String())
		}
		for _, field := range strings.Fields(processes[index].stdout.String()) {
			taskID, err := strconv.ParseInt(field, 10, 64)
			if err != nil {
				return fmt.Errorf("parse worker %d output %q: %w", index, field, err)
			}
			claims[taskID]++
		}
	}

	duplicates := 0
	totalClaims := 0
	for _, count := range claims {
		totalClaims += count
		if count > 1 {
			duplicates += count - 1
		}
	}
	fmt.Printf("OS processes: %d\n", options.workers)
	fmt.Printf("Tasks attacked: %d\n", options.tasks)
	fmt.Printf("Total claims: %d\n", totalClaims)
	fmt.Printf("Unique tasks claimed: %d\n", len(claims))
	fmt.Printf("Duplicate claims: %d\n", duplicates)
	if duplicates != 0 || totalClaims != options.tasks || len(claims) != options.tasks {
		return errors.New("claim race assertions failed")
	}
	fmt.Println("RESULT: PASS - every task had exactly one owner")
	return nil
}

func runChild(ctx context.Context, options options) error {
	dataStore, err := store.Open(ctx, options.databaseURL)
	if err != nil {
		return err
	}
	defer dataStore.Close()
	if wait := time.Until(time.Unix(0, options.startAt)); wait > 0 {
		time.Sleep(wait)
	}
	workerID := fmt.Sprintf("process-%d", os.Getpid())
	for {
		claimed, err := dataStore.ClaimNext(ctx, workerID, time.Hour)
		if err != nil {
			return err
		}
		if claimed == nil {
			return nil
		}
		fmt.Println(claimed.ID)
	}
}

func replaceDatabase(databaseURL, name string) (string, error) {
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		return "", fmt.Errorf("parse database URL: %w", err)
	}
	parsed.Path = "/" + name
	return parsed.String(), nil
}
