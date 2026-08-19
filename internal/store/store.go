package store

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"taskboard/internal/parameters"
)

var (
	ErrNotFound       = errors.New("not found")
	ErrNotOwner       = errors.New("task is owned by another worker")
	ErrLeaseLost      = errors.New("worker lease is no longer valid")
	ErrInvalidState   = errors.New("invalid task or step state")
	ErrTaskTerminal   = errors.New("task is in a terminal state")
	ErrNoSteps        = errors.New("task has no steps")
	ErrInvalidPayload = errors.New("invalid payload")
)

//go:embed migrations/*.sql
var migrations embed.FS

type Store struct {
	pool *pgxpool.Pool
}

type StepInput struct {
	Overrides parameters.Values `json:"overrides"`
}

type CreateTaskInput struct {
	GroupName      string            `json:"group_name"`
	GroupOverrides parameters.Values `json:"group_overrides"`
	BaseParameters parameters.Values `json:"base_parameters"`
	Steps          []StepInput       `json:"steps"`
	DemoClaim      bool              `json:"-"`
}

type ClaimedTask struct {
	ID                  int64             `json:"id"`
	WorkerID            string            `json:"worker_id"`
	FencingToken        int64             `json:"fencing_token"`
	LeaseExpiresAt      time.Time         `json:"lease_expires_at"`
	Recovered           bool              `json:"recovered"`
	EffectiveParameters parameters.Values `json:"effective_parameters"`
}

type RunningStep struct {
	TaskID             int64             `json:"task_id"`
	Position           int               `json:"position"`
	IdempotencyKey     string            `json:"idempotency_key"`
	ResolvedParameters parameters.Values `json:"resolved_parameters"`
}

type StepLog struct {
	CompletedAt time.Time `json:"completed_at"`
	Success     bool      `json:"success"`
}

type StepView struct {
	Position           int               `json:"position"`
	Status             string            `json:"status"`
	Overrides          parameters.Values `json:"overrides"`
	ResolvedParameters parameters.Values `json:"resolved_parameters,omitempty"`
	StartedAt          *time.Time        `json:"started_at,omitempty"`
	Log                *StepLog          `json:"log,omitempty"`
}

type TaskView struct {
	ID                      int64             `json:"id"`
	GroupName               string            `json:"group_name"`
	GroupOverrides          parameters.Values `json:"group_overrides"`
	BaseParameters          parameters.Values `json:"base_parameters"`
	GroupParametersSnapshot parameters.Values `json:"group_parameters_snapshot,omitempty"`
	EffectiveParameters     parameters.Values `json:"effective_parameters,omitempty"`
	Status                  string            `json:"status"`
	ClaimedBy               *string           `json:"claimed_by,omitempty"`
	ClaimedAt               *time.Time        `json:"claimed_at,omitempty"`
	FencingToken            int64             `json:"fencing_token"`
	LeaseExpiresAt          *time.Time        `json:"lease_expires_at,omitempty"`
	CurrentStep             *int              `json:"current_step,omitempty"`
	CreatedAt               time.Time         `json:"created_at"`
	UpdatedAt               time.Time         `json:"updated_at"`
	Steps                   []StepView        `json:"steps"`
}

type TaskSummary struct {
	Total    int `json:"total"`
	Pending  int `json:"pending"`
	Active   int `json:"active"`
	Finished int `json:"finished"`
}

type TaskPage struct {
	Tasks   []TaskView  `json:"tasks"`
	Summary TaskSummary `json:"summary"`
	Limit   int         `json:"limit"`
	Offset  int         `json:"offset"`
}

type CompletionResult struct {
	TaskID          int64     `json:"task_id"`
	StepPosition    int       `json:"step_position"`
	Success         bool      `json:"success"`
	TaskStatus      string    `json:"task_status"`
	StepStatus      string    `json:"step_status"`
	DuplicateReport bool      `json:"duplicate_report"`
	LogCount        int       `json:"log_count"`
	ReceivedAt      time.Time `json:"received_at,omitempty"`
}

type ActivityLog struct {
	ID           int64             `json:"id"`
	TaskID       int64             `json:"task_id"`
	StepPosition *int              `json:"step_position,omitempty"`
	EventType    string            `json:"event_type"`
	WorkerID     *string           `json:"worker_id,omitempty"`
	Details      parameters.Values `json:"details"`
	CreatedAt    time.Time         `json:"created_at"`
}

func Open(ctx context.Context, databaseURL string) (*Store, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database URL: %w", err)
	}
	config.MaxConns = 20
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() {
	s.pool.Close()
}

func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

func (s *Store) Migrate(ctx context.Context) error {
	connection, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer connection.Release()
	if _, err := connection.Exec(ctx, `SELECT pg_advisory_lock(hashtext('taskboard_migrations'))`); err != nil {
		return fmt.Errorf("lock migrations: %w", err)
	}
	defer func() {
		_, _ = connection.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtext('taskboard_migrations'))`)
	}()
	if _, err := connection.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)
	`); err != nil {
		return fmt.Errorf("create migration ledger: %w", err)
	}

	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		var applied bool
		if err := connection.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, entry.Name(),
		).Scan(&applied); err != nil {
			return fmt.Errorf("check migration %s: %w", entry.Name(), err)
		}
		if applied {
			continue
		}
		sql, err := migrations.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return fmt.Errorf("read migration %s: %w", entry.Name(), err)
		}
		tx, err := connection.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", entry.Name(), err)
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply migration %s: %w", entry.Name(), err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (version) VALUES ($1)`, entry.Name(),
		); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("record migration %s: %w", entry.Name(), err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit migration %s: %w", entry.Name(), err)
		}
	}
	return nil
}

func (s *Store) CreateTask(ctx context.Context, input CreateTaskInput) (int64, error) {
	input.GroupName = strings.TrimSpace(input.GroupName)
	if input.GroupName == "" || len(input.Steps) == 0 {
		return 0, fmt.Errorf("%w: group_name and at least one step are required", ErrInvalidPayload)
	}
	if input.GroupOverrides == nil {
		input.GroupOverrides = parameters.Values{}
	}
	if input.BaseParameters == nil {
		input.BaseParameters = parameters.Values{}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	groupJSON, err := marshalValues(input.GroupOverrides)
	if err != nil {
		return 0, fmt.Errorf("%w: group overrides: %v", ErrInvalidPayload, err)
	}
	var groupID int64
	if err := tx.QueryRow(ctx,
		`INSERT INTO task_groups (name, overrides) VALUES ($1, $2::jsonb) RETURNING id`,
		input.GroupName, groupJSON,
	).Scan(&groupID); err != nil {
		return 0, fmt.Errorf("insert group: %w", err)
	}

	baseJSON, err := marshalValues(input.BaseParameters)
	if err != nil {
		return 0, fmt.Errorf("%w: base parameters: %v", ErrInvalidPayload, err)
	}
	var taskID int64
	if err := tx.QueryRow(ctx,
		`INSERT INTO tasks (group_id, base_parameters, demo_claim) VALUES ($1, $2::jsonb, $3) RETURNING id`,
		groupID, baseJSON, input.DemoClaim,
	).Scan(&taskID); err != nil {
		return 0, fmt.Errorf("insert task: %w", err)
	}

	for index, step := range input.Steps {
		if step.Overrides == nil {
			step.Overrides = parameters.Values{}
		}
		overridesJSON, err := marshalValues(step.Overrides)
		if err != nil {
			return 0, fmt.Errorf("%w: step %d overrides: %v", ErrInvalidPayload, index+1, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO steps (task_id, position, overrides) VALUES ($1, $2, $3::jsonb)`,
			taskID, index+1, overridesJSON,
		); err != nil {
			return 0, fmt.Errorf("insert step %d: %w", index+1, err)
		}
	}
	if err := writeActivity(ctx, tx, taskID, nil, "task_created", nil, parameters.Values{
		"group_name": input.GroupName,
		"step_count": len(input.Steps),
	}); err != nil {
		return 0, err
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit task: %w", err)
	}
	return taskID, nil
}

// ClaimNext uses a row lock and SKIP LOCKED so concurrent database clients cannot
// observe the same pending or expired task as claimable. Every claim increments a
// fencing token, which prevents a recovered task's previous owner from writing.
func (s *Store) ClaimNext(ctx context.Context, workerID string, leaseDuration time.Duration) (*ClaimedTask, error) {
	workerID = strings.TrimSpace(workerID)
	if workerID == "" || leaseDuration <= 0 {
		return nil, fmt.Errorf("%w: worker ID and positive lease duration are required", ErrInvalidPayload)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var taskID int64
	var previousStatus string
	var previousWorker *string
	var baseJSON, groupSnapshotJSON, groupJSON, effectiveJSON []byte
	err = tx.QueryRow(ctx, `
		SELECT t.id, t.status, t.claimed_by, t.base_parameters,
		       COALESCE(t.group_parameters_snapshot, '{}'::jsonb), g.overrides,
		       COALESCE(t.effective_parameters, '{}'::jsonb)
		FROM tasks t
		JOIN task_groups g ON g.id = t.group_id
		WHERE NOT t.demo_claim AND (
			t.status = 'pending'
			OR (t.status IN ('claimed', 'running') AND t.lease_expires_at < now())
		)
		ORDER BY t.id
		FOR UPDATE OF t SKIP LOCKED
		LIMIT 1
	`).Scan(
		&taskID, &previousStatus, &previousWorker, &baseJSON,
		&groupSnapshotJSON, &groupJSON, &effectiveJSON,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("select claim candidate: %w", err)
	}

	recovered := previousStatus != "pending"
	var effective parameters.Values
	if recovered {
		effective, err = unmarshalValues(effectiveJSON)
		if err != nil {
			return nil, err
		}
	} else {
		base, decodeErr := unmarshalValues(baseJSON)
		if decodeErr != nil {
			return nil, decodeErr
		}
		group, decodeErr := unmarshalValues(groupJSON)
		if decodeErr != nil {
			return nil, decodeErr
		}
		effective = parameters.AtTaskStart(base, group)
		groupSnapshotJSON, err = marshalValues(group)
		if err != nil {
			return nil, fmt.Errorf("encode group snapshot: %w", err)
		}
		effectiveJSON, err = marshalValues(effective)
		if err != nil {
			return nil, fmt.Errorf("encode effective parameters: %w", err)
		}
	}

	var fencingToken int64
	var leaseExpiresAt time.Time
	if err := tx.QueryRow(ctx, `
		UPDATE tasks
		SET status = 'claimed', claimed_by = $2, claimed_at = now(),
		    group_parameters_snapshot = $3::jsonb, effective_parameters = $4::jsonb,
		    ownership_version = ownership_version + 1,
		    lease_expires_at = now() + ($5 * interval '1 millisecond'), updated_at = now()
		WHERE id = $1
		RETURNING ownership_version, lease_expires_at
	`, taskID, workerID, groupSnapshotJSON, effectiveJSON, leaseDuration.Milliseconds()).Scan(
		&fencingToken, &leaseExpiresAt,
	); err != nil {
		return nil, fmt.Errorf("claim task: %w", err)
	}
	eventType := "task_claimed"
	details := parameters.Values{"fencing_token": fencingToken, "lease_expires_at": leaseExpiresAt}
	if recovered {
		eventType = "task_reclaimed"
		details["previous_worker"] = previousWorker
	}
	if err := writeActivity(ctx, tx, taskID, nil, eventType, &workerID, details); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit claim: %w", err)
	}
	return &ClaimedTask{
		ID: taskID, WorkerID: workerID, FencingToken: fencingToken,
		LeaseExpiresAt: leaseExpiresAt, Recovered: recovered, EffectiveParameters: effective,
	}, nil
}

func (s *Store) CreateClaimDemoTask(ctx context.Context) (int64, error) {
	return s.CreateTask(ctx, CreateTaskInput{
		GroupName:      "并发认领演示",
		GroupOverrides: parameters.Values{"scope": "claim-demo"},
		BaseParameters: parameters.Values{"purpose": "single-owner-proof"},
		Steps:          []StepInput{{Overrides: parameters.Values{}}},
		DemoClaim:      true,
	})
}

func (s *Store) RenewLease(
	ctx context.Context,
	taskID int64,
	workerID string,
	fencingToken int64,
	leaseDuration time.Duration,
) (time.Time, error) {
	if leaseDuration <= 0 {
		return time.Time{}, fmt.Errorf("%w: positive lease duration is required", ErrInvalidPayload)
	}
	var expiresAt time.Time
	err := s.pool.QueryRow(ctx, `
		UPDATE tasks
		SET lease_expires_at = now() + ($4 * interval '1 millisecond'), updated_at = now()
		WHERE id = $1 AND claimed_by = $2 AND ownership_version = $3
		  AND status IN ('claimed', 'running') AND lease_expires_at > now()
		RETURNING lease_expires_at
	`, taskID, workerID, fencingToken, leaseDuration.Milliseconds()).Scan(&expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, ErrLeaseLost
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("renew task lease: %w", err)
	}
	return expiresAt, nil
}

// CancelTask atomically moves an active task and all unfinished steps into a
// terminal state. In-flight workers are fenced because their lease is cleared.
func (s *Store) CancelTask(ctx context.Context, taskID int64, actor string) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM tasks WHERE id = $1 FOR UPDATE`, taskID).Scan(&status); errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotFound
	} else if err != nil {
		return false, fmt.Errorf("lock task for cancellation: %w", err)
	}
	if status == "cancelled" {
		return false, nil
	}
	if status == "done" || status == "failed" {
		return false, ErrTaskTerminal
	}
	if _, err := tx.Exec(ctx, `
		UPDATE steps SET status = 'cancelled'
		WHERE task_id = $1 AND status IN ('pending', 'running')
	`, taskID); err != nil {
		return false, fmt.Errorf("cancel unfinished steps: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE tasks
		SET status = 'cancelled', lease_expires_at = NULL, updated_at = now()
		WHERE id = $1
	`, taskID); err != nil {
		return false, fmt.Errorf("cancel task: %w", err)
	}
	if err := writeActivity(ctx, tx, taskID, nil, "task_cancelled", nil, parameters.Values{
		"actor": actor,
	}); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit cancellation: %w", err)
	}
	return true, nil
}

// ClaimSpecificDemo applies the same transaction and row-lock ownership rule as
// ClaimNext, but targets a demo-only task that normal workers cannot see.
func (s *Store) ClaimSpecificDemo(ctx context.Context, taskID int64, workerID string) (bool, int32, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var backendPID int32
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&backendPID); err != nil {
		return false, 0, fmt.Errorf("read demo database session: %w", err)
	}

	var baseJSON, groupJSON []byte
	err = tx.QueryRow(ctx, `
		SELECT t.base_parameters, g.overrides
		FROM tasks t
		JOIN task_groups g ON g.id = t.group_id
		WHERE t.id = $1 AND t.status = 'pending' AND t.demo_claim
		FOR UPDATE OF t
	`, taskID).Scan(&baseJSON, &groupJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, backendPID, nil
	}
	if err != nil {
		return false, backendPID, fmt.Errorf("lock demo task: %w", err)
	}
	base, err := unmarshalValues(baseJSON)
	if err != nil {
		return false, backendPID, err
	}
	group, err := unmarshalValues(groupJSON)
	if err != nil {
		return false, backendPID, err
	}
	effective := parameters.AtTaskStart(base, group)
	groupSnapshotJSON, err := marshalValues(group)
	if err != nil {
		return false, backendPID, fmt.Errorf("encode demo group snapshot: %w", err)
	}
	effectiveJSON, err := marshalValues(effective)
	if err != nil {
		return false, backendPID, fmt.Errorf("encode demo effective parameters: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE tasks
		SET status = 'claimed', claimed_by = $2, claimed_at = now(),
		    group_parameters_snapshot = $3::jsonb, effective_parameters = $4::jsonb,
		    updated_at = now()
		WHERE id = $1
	`, taskID, workerID, groupSnapshotJSON, effectiveJSON); err != nil {
		return false, backendPID, fmt.Errorf("claim demo task: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, backendPID, fmt.Errorf("commit demo claim: %w", err)
	}
	return true, backendPID, nil
}

func (s *Store) RecordClaimDemoAttempt(
	ctx context.Context,
	taskID int64,
	workerID string,
	won bool,
	winner string,
	duration time.Duration,
) error {
	detailsJSON, err := marshalValues(parameters.Values{
		"claim_demo":  true,
		"won":         won,
		"winner":      winner,
		"duration_ms": float64(duration.Microseconds()) / 1000,
	})
	if err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO activity_logs (task_id, event_type, worker_id, details)
		VALUES ($1, 'task_claimed', $2, $3::jsonb)
	`, taskID, workerID, detailsJSON); err != nil {
		return fmt.Errorf("record claim demo attempt: %w", err)
	}
	return nil
}

func (s *Store) FinishClaimDemo(ctx context.Context, taskID int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		UPDATE tasks SET status = 'done', updated_at = now()
		WHERE id = $1 AND demo_claim
	`, taskID); err != nil {
		return fmt.Errorf("finish claim demo: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM tasks
		WHERE id IN (
			SELECT id FROM tasks
			WHERE demo_claim AND status = 'done'
			ORDER BY id DESC
			OFFSET 10
		)
	`); err != nil {
		return fmt.Errorf("prune claim demos: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM task_groups g
		WHERE NOT EXISTS (SELECT 1 FROM tasks t WHERE t.group_id = g.id)
	`); err != nil {
		return fmt.Errorf("prune orphan demo groups: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit claim demo: %w", err)
	}
	return nil
}

func (s *Store) StartCurrentStep(ctx context.Context, taskID int64, workerID string, fencingToken int64) (*RunningStep, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var status string
	var claimedBy *string
	var currentFencingToken int64
	var leaseValid bool
	var currentStep *int
	var effectiveJSON []byte
	if err := tx.QueryRow(ctx, `
		SELECT status, claimed_by, ownership_version,
		       COALESCE(lease_expires_at > now(), false),
		       current_step, effective_parameters
		FROM tasks WHERE id = $1 FOR UPDATE
	`, taskID).Scan(
		&status, &claimedBy, &currentFencingToken, &leaseValid, &currentStep, &effectiveJSON,
	); errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, fmt.Errorf("lock task: %w", err)
	}
	if status == "done" || status == "failed" || status == "cancelled" {
		return nil, ErrTaskTerminal
	}
	if claimedBy == nil || *claimedBy != workerID || currentFencingToken != fencingToken ||
		!leaseValid {
		return nil, ErrLeaseLost
	}

	position := 1
	if currentStep != nil {
		position = *currentStep
	}
	var stepStatus string
	var overridesJSON []byte
	var resolvedJSON []byte
	err = tx.QueryRow(ctx, `
		SELECT status, overrides, COALESCE(resolved_parameters, '{}'::jsonb)
		FROM steps WHERE task_id = $1 AND position = $2 FOR UPDATE
	`, taskID, position).Scan(&stepStatus, &overridesJSON, &resolvedJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoSteps
	}
	if err != nil {
		return nil, fmt.Errorf("lock step: %w", err)
	}

	if stepStatus == "running" {
		resolved, err := unmarshalValues(resolvedJSON)
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE tasks SET status = 'running', updated_at = now()
			WHERE id = $1
		`, taskID); err != nil {
			return nil, fmt.Errorf("mark resumed task running: %w", err)
		}
		if err := writeActivity(ctx, tx, taskID, &position, "step_started", &workerID, parameters.Values{
			"resolved_parameters": resolved,
			"resumed":             true,
			"idempotency_key":     stepIdempotencyKey(taskID, position),
		}); err != nil {
			return nil, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return &RunningStep{
			TaskID: taskID, Position: position, IdempotencyKey: stepIdempotencyKey(taskID, position),
			ResolvedParameters: resolved,
		}, nil
	}
	if stepStatus != "pending" {
		return nil, fmt.Errorf("%w: step %d is %s", ErrInvalidState, position, stepStatus)
	}

	current, err := unmarshalValues(effectiveJSON)
	if err != nil {
		return nil, err
	}
	overrides, err := unmarshalValues(overridesJSON)
	if err != nil {
		return nil, err
	}
	resolved := parameters.AtStep(current, overrides)
	resolvedJSON, err = marshalValues(resolved)
	if err != nil {
		return nil, fmt.Errorf("encode resolved step parameters: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE steps SET status = 'running', resolved_parameters = $3::jsonb, started_at = now()
		WHERE task_id = $1 AND position = $2
	`, taskID, position, resolvedJSON); err != nil {
		return nil, fmt.Errorf("start step: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE tasks SET status = 'running', current_step = $2,
		    effective_parameters = $3::jsonb, updated_at = now()
		WHERE id = $1
	`, taskID, position, resolvedJSON); err != nil {
		return nil, fmt.Errorf("mark task running: %w", err)
	}
	if err := writeActivity(ctx, tx, taskID, &position, "step_started", &workerID, parameters.Values{
		"resolved_parameters": resolved,
		"idempotency_key":     stepIdempotencyKey(taskID, position),
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit step start: %w", err)
	}
	return &RunningStep{
		TaskID: taskID, Position: position, IdempotencyKey: stepIdempotencyKey(taskID, position),
		ResolvedParameters: resolved,
	}, nil
}

func (s *Store) CompleteStep(
	ctx context.Context,
	taskID int64,
	position int,
	workerID string,
	fencingToken int64,
	success bool,
) (*CompletionResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var taskStatus string
	var currentStep *int
	var claimedBy *string
	var currentFencingToken int64
	var leaseValid bool
	if err := tx.QueryRow(ctx,
		`SELECT status, current_step, claimed_by, ownership_version,
		        COALESCE(lease_expires_at > now(), false)
		 FROM tasks WHERE id = $1 FOR UPDATE`, taskID,
	).Scan(&taskStatus, &currentStep, &claimedBy, &currentFencingToken, &leaseValid); errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, fmt.Errorf("lock task: %w", err)
	}
	if taskStatus == "cancelled" {
		return nil, ErrTaskTerminal
	}
	if claimedBy == nil || *claimedBy != workerID || currentFencingToken != fencingToken {
		return nil, ErrLeaseLost
	}
	if taskStatus != "done" && taskStatus != "failed" && !leaseValid {
		return nil, ErrLeaseLost
	}

	var stepStatus string
	if err := tx.QueryRow(ctx,
		`SELECT status FROM steps WHERE task_id = $1 AND position = $2 FOR UPDATE`, taskID, position,
	).Scan(&stepStatus); errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, fmt.Errorf("lock step: %w", err)
	}
	if stepStatus == "pending" || (stepStatus != "done" && (currentStep == nil || *currentStep != position)) {
		return nil, fmt.Errorf("%w: step %d is not the current running step", ErrInvalidState, position)
	}
	var duplicateReport bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM step_logs WHERE task_id = $1 AND step_position = $2
		)
	`, taskID, position).Scan(&duplicateReport); err != nil {
		return nil, fmt.Errorf("check existing step log: %w", err)
	}

	var finalSuccess bool
	if err := tx.QueryRow(ctx, `
		INSERT INTO step_logs (task_id, step_position, success)
		VALUES ($1, $2, $3)
		ON CONFLICT (task_id, step_position) DO UPDATE
		SET success = step_logs.success OR EXCLUDED.success,
		    completed_at = CASE
		        WHEN NOT step_logs.success AND EXCLUDED.success THEN EXCLUDED.completed_at
		        ELSE step_logs.completed_at
		    END
		RETURNING success
	`, taskID, position, success).Scan(&finalSuccess); err != nil {
		return nil, fmt.Errorf("write step log: %w", err)
	}

	taskCompleted := false
	taskFailed := false
	if finalSuccess && stepStatus != "done" {
		stepStatus = "done"
		if _, err := tx.Exec(ctx,
			`UPDATE steps SET status = 'done' WHERE task_id = $1 AND position = $2`, taskID, position,
		); err != nil {
			return nil, fmt.Errorf("complete step: %w", err)
		}

		var nextPosition int
		err := tx.QueryRow(ctx, `
			SELECT position FROM steps
			WHERE task_id = $1 AND position > $2
			ORDER BY position LIMIT 1
		`, taskID, position).Scan(&nextPosition)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			taskStatus = "done"
			taskCompleted = true
			_, err = tx.Exec(ctx,
				`UPDATE tasks SET status = 'done', current_step = $2,
					 lease_expires_at = NULL, updated_at = now() WHERE id = $1`,
				taskID, position,
			)
		case err != nil:
			return nil, fmt.Errorf("find next step: %w", err)
		default:
			taskStatus = "claimed"
			_, err = tx.Exec(ctx,
				`UPDATE tasks SET status = 'claimed', current_step = $2, updated_at = now() WHERE id = $1`,
				taskID, nextPosition,
			)
		}
		if err != nil {
			return nil, fmt.Errorf("advance task: %w", err)
		}
	} else if !finalSuccess && stepStatus == "running" {
		stepStatus = "failed"
		taskStatus = "failed"
		taskFailed = true
		if _, err := tx.Exec(ctx,
			`UPDATE steps SET status = 'failed' WHERE task_id = $1 AND position = $2`, taskID, position,
		); err != nil {
			return nil, fmt.Errorf("fail step: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE tasks SET status = 'failed', updated_at = now() WHERE id = $1`, taskID,
		); err != nil {
			return nil, fmt.Errorf("fail task: %w", err)
		}
	}
	var logCount int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM step_logs WHERE task_id = $1 AND step_position = $2
	`, taskID, position).Scan(&logCount); err != nil {
		return nil, fmt.Errorf("count step logs: %w", err)
	}
	if err := writeActivity(ctx, tx, taskID, &position, "step_reported", &workerID, parameters.Values{
		"reported_success": success,
		"final_success":    finalSuccess,
		"duplicate_report": duplicateReport,
		"log_count":        logCount,
		"fencing_token":    fencingToken,
	}); err != nil {
		return nil, err
	}
	if taskCompleted {
		if err := writeActivity(ctx, tx, taskID, nil, "task_done", nil, parameters.Values{}); err != nil {
			return nil, err
		}
	}
	if taskFailed {
		if err := writeActivity(ctx, tx, taskID, &position, "task_failed", nil, parameters.Values{}); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit completion: %w", err)
	}
	return &CompletionResult{
		TaskID: taskID, StepPosition: position, Success: finalSuccess,
		TaskStatus: taskStatus, StepStatus: stepStatus,
		DuplicateReport: duplicateReport, LogCount: logCount,
	}, nil
}

func (s *Store) ListActivity(ctx context.Context, limit int) ([]ActivityLog, error) {
	if limit <= 0 || limit > 200 {
		limit = 60
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, task_id, step_position, event_type, worker_id, details, created_at
		FROM activity_logs
		ORDER BY id DESC
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("list activity logs: %w", err)
	}
	defer rows.Close()

	logs := make([]ActivityLog, 0)
	for rows.Next() {
		var log ActivityLog
		var detailsJSON []byte
		if err := rows.Scan(
			&log.ID, &log.TaskID, &log.StepPosition, &log.EventType,
			&log.WorkerID, &detailsJSON, &log.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan activity log: %w", err)
		}
		log.Details, err = unmarshalValues(detailsJSON)
		if err != nil {
			return nil, fmt.Errorf("decode activity %d details: %w", log.ID, err)
		}
		logs = append(logs, log)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate activity logs: %w", err)
	}
	return logs, nil
}

func (s *Store) ListTasks(ctx context.Context, limit, offset int) (*TaskPage, error) {
	if limit <= 0 || limit > 200 {
		limit = 25
	}
	if offset < 0 {
		offset = 0
	}
	page := &TaskPage{Tasks: []TaskView{}, Limit: limit, Offset: offset}
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE status = 'pending'),
		       count(*) FILTER (WHERE status IN ('claimed', 'running')),
		       count(*) FILTER (WHERE status IN ('done', 'failed', 'cancelled'))
		FROM tasks
		WHERE NOT demo_claim
	`).Scan(
		&page.Summary.Total, &page.Summary.Pending,
		&page.Summary.Active, &page.Summary.Finished,
	); err != nil {
		return nil, fmt.Errorf("summarize tasks: %w", err)
	}

	rows, err := s.pool.Query(ctx, `
		SELECT t.id, g.name, g.overrides, t.base_parameters,
		       COALESCE(t.group_parameters_snapshot, '{}'::jsonb),
		       COALESCE(t.effective_parameters, '{}'::jsonb),
		       t.status, t.claimed_by, t.claimed_at, t.ownership_version,
		       t.lease_expires_at, t.current_step,
		       t.created_at, t.updated_at
		FROM tasks t JOIN task_groups g ON g.id = t.group_id
		WHERE NOT t.demo_claim
		ORDER BY t.id DESC
		LIMIT $1 OFFSET $2
	`, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	defer rows.Close()

	indexes := make(map[int64]int)
	taskIDs := make([]int64, 0, limit)
	for rows.Next() {
		var task TaskView
		var groupJSON, baseJSON, snapshotJSON, effectiveJSON []byte
		if err := rows.Scan(
			&task.ID, &task.GroupName, &groupJSON, &baseJSON, &snapshotJSON, &effectiveJSON,
			&task.Status, &task.ClaimedBy, &task.ClaimedAt, &task.FencingToken,
			&task.LeaseExpiresAt, &task.CurrentStep,
			&task.CreatedAt, &task.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan task: %w", err)
		}
		task.GroupOverrides, err = unmarshalValues(groupJSON)
		if err != nil {
			return nil, fmt.Errorf("decode task %d group parameters: %w", task.ID, err)
		}
		task.BaseParameters, err = unmarshalValues(baseJSON)
		if err != nil {
			return nil, fmt.Errorf("decode task %d base parameters: %w", task.ID, err)
		}
		task.GroupParametersSnapshot, err = unmarshalValues(snapshotJSON)
		if err != nil {
			return nil, fmt.Errorf("decode task %d group snapshot: %w", task.ID, err)
		}
		task.EffectiveParameters, err = unmarshalValues(effectiveJSON)
		if err != nil {
			return nil, fmt.Errorf("decode task %d effective parameters: %w", task.ID, err)
		}
		task.Steps = []StepView{}
		indexes[task.ID] = len(page.Tasks)
		taskIDs = append(taskIDs, task.ID)
		page.Tasks = append(page.Tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tasks: %w", err)
	}
	if len(page.Tasks) == 0 {
		return page, nil
	}

	stepRows, err := s.pool.Query(ctx, `
		SELECT s.task_id, s.position, s.status, s.overrides,
		       COALESCE(s.resolved_parameters, '{}'::jsonb), s.started_at,
		       l.completed_at, l.success
		FROM steps s
		LEFT JOIN step_logs l ON l.task_id = s.task_id AND l.step_position = s.position
		WHERE s.task_id = ANY($1::bigint[])
		ORDER BY s.task_id DESC, s.position
	`, taskIDs)
	if err != nil {
		return nil, fmt.Errorf("list steps: %w", err)
	}
	defer stepRows.Close()
	for stepRows.Next() {
		var taskID int64
		var step StepView
		var overridesJSON, resolvedJSON []byte
		var completedAt *time.Time
		var success *bool
		if err := stepRows.Scan(
			&taskID, &step.Position, &step.Status, &overridesJSON, &resolvedJSON,
			&step.StartedAt, &completedAt, &success,
		); err != nil {
			return nil, fmt.Errorf("scan step: %w", err)
		}
		index, exists := indexes[taskID]
		if !exists {
			continue
		}
		step.Overrides, err = unmarshalValues(overridesJSON)
		if err != nil {
			return nil, fmt.Errorf("decode task %d step %d overrides: %w", taskID, step.Position, err)
		}
		step.ResolvedParameters, err = unmarshalValues(resolvedJSON)
		if err != nil {
			return nil, fmt.Errorf("decode task %d step %d resolved parameters: %w", taskID, step.Position, err)
		}
		if completedAt != nil && success != nil {
			step.Log = &StepLog{CompletedAt: *completedAt, Success: *success}
		}
		page.Tasks[index].Steps = append(page.Tasks[index].Steps, step)
	}
	if err := stepRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate steps: %w", err)
	}
	return page, nil
}

// PruneTerminalTasks removes a bounded batch of expired terminal tasks. All
// dependent steps and logs are deleted through foreign keys in the same
// transaction; active tasks are never eligible.
func (s *Store) PruneTerminalTasks(ctx context.Context, before time.Time, batch int) (int64, error) {
	if batch <= 0 || batch > 5000 {
		batch = 1000
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	result, err := tx.Exec(ctx, `
		WITH victims AS (
			SELECT id FROM tasks
			WHERE NOT demo_claim
			  AND status IN ('done', 'failed', 'cancelled')
			  AND updated_at < $1
			ORDER BY id
			LIMIT $2
		)
		DELETE FROM tasks t USING victims v WHERE t.id = v.id
	`, before, batch)
	if err != nil {
		return 0, fmt.Errorf("prune terminal tasks: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM task_groups g
		WHERE NOT EXISTS (SELECT 1 FROM tasks t WHERE t.group_id = g.id)
	`); err != nil {
		return 0, fmt.Errorf("prune orphan groups: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit task retention: %w", err)
	}
	return result.RowsAffected(), nil
}

func stepIdempotencyKey(taskID int64, position int) string {
	return fmt.Sprintf("task:%d:step:%d", taskID, position)
}

func marshalValues(values parameters.Values) ([]byte, error) {
	if values == nil {
		values = parameters.Values{}
	}
	return json.Marshal(values)
}

func unmarshalValues(data []byte) (parameters.Values, error) {
	values := parameters.Values{}
	if len(data) == 0 {
		return values, nil
	}
	if err := json.Unmarshal(data, &values); err != nil {
		return nil, fmt.Errorf("decode parameters: %w", err)
	}
	return values, nil
}

func writeActivity(
	ctx context.Context,
	tx pgx.Tx,
	taskID int64,
	stepPosition *int,
	eventType string,
	workerID *string,
	details parameters.Values,
) error {
	detailsJSON, err := marshalValues(details)
	if err != nil {
		return fmt.Errorf("encode activity details: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO activity_logs (task_id, step_position, event_type, worker_id, details)
		VALUES ($1, $2, $3, $4, $5::jsonb)
	`, taskID, stepPosition, eventType, workerID, detailsJSON); err != nil {
		return fmt.Errorf("write activity log: %w", err)
	}
	return nil
}
