ALTER TABLE tasks
    ADD COLUMN IF NOT EXISTS ownership_version BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS lease_expires_at TIMESTAMPTZ;

UPDATE tasks
SET lease_expires_at = now()
WHERE status IN ('claimed', 'running') AND lease_expires_at IS NULL AND NOT demo_claim;

ALTER TABLE tasks DROP CONSTRAINT IF EXISTS tasks_status_valid;
ALTER TABLE tasks ADD CONSTRAINT tasks_status_valid
    CHECK (status IN ('pending', 'claimed', 'running', 'done', 'failed', 'cancelled'));

ALTER TABLE steps DROP CONSTRAINT IF EXISTS steps_status_valid;
ALTER TABLE steps ADD CONSTRAINT steps_status_valid
    CHECK (status IN ('pending', 'running', 'done', 'failed', 'cancelled'));

ALTER TABLE activity_logs DROP CONSTRAINT IF EXISTS activity_logs_event_valid;
ALTER TABLE activity_logs ADD CONSTRAINT activity_logs_event_valid CHECK (event_type IN (
    'task_created', 'task_claimed', 'task_reclaimed', 'step_started', 'step_reported',
    'task_done', 'task_failed', 'task_cancelled'
));

CREATE INDEX IF NOT EXISTS tasks_lease_recovery_idx
    ON tasks (lease_expires_at, id)
    WHERE status IN ('claimed', 'running') AND NOT demo_claim;
