CREATE TABLE IF NOT EXISTS task_groups (
    id BIGSERIAL PRIMARY KEY,
    name TEXT NOT NULL,
    overrides JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT task_groups_overrides_object CHECK (jsonb_typeof(overrides) = 'object')
);

CREATE TABLE IF NOT EXISTS tasks (
    id BIGSERIAL PRIMARY KEY,
    group_id BIGINT NOT NULL REFERENCES task_groups(id),
    base_parameters JSONB NOT NULL DEFAULT '{}'::jsonb,
    group_parameters_snapshot JSONB,
    effective_parameters JSONB,
    demo_claim BOOLEAN NOT NULL DEFAULT false,
    status TEXT NOT NULL DEFAULT 'pending',
    claimed_by TEXT,
    claimed_at TIMESTAMPTZ,
    current_step INTEGER,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT tasks_status_valid CHECK (status IN ('pending', 'claimed', 'running', 'done', 'failed')),
    CONSTRAINT tasks_base_parameters_object CHECK (jsonb_typeof(base_parameters) = 'object'),
    CONSTRAINT tasks_group_snapshot_object CHECK (group_parameters_snapshot IS NULL OR jsonb_typeof(group_parameters_snapshot) = 'object'),
    CONSTRAINT tasks_effective_parameters_object CHECK (effective_parameters IS NULL OR jsonb_typeof(effective_parameters) = 'object')
);

ALTER TABLE tasks ADD COLUMN IF NOT EXISTS demo_claim BOOLEAN NOT NULL DEFAULT false;

CREATE INDEX IF NOT EXISTS tasks_claim_order_idx ON tasks (status, id);

CREATE TABLE IF NOT EXISTS steps (
    id BIGSERIAL PRIMARY KEY,
    task_id BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    position INTEGER NOT NULL,
    overrides JSONB NOT NULL DEFAULT '{}'::jsonb,
    resolved_parameters JSONB,
    status TEXT NOT NULL DEFAULT 'pending',
    started_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT steps_position_positive CHECK (position > 0),
    CONSTRAINT steps_status_valid CHECK (status IN ('pending', 'running', 'done', 'failed')),
    CONSTRAINT steps_overrides_object CHECK (jsonb_typeof(overrides) = 'object'),
    CONSTRAINT steps_resolved_parameters_object CHECK (resolved_parameters IS NULL OR jsonb_typeof(resolved_parameters) = 'object'),
    UNIQUE (task_id, position)
);

CREATE TABLE IF NOT EXISTS step_logs (
    id BIGSERIAL PRIMARY KEY,
    task_id BIGINT NOT NULL,
    step_position INTEGER NOT NULL,
    completed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    success BOOLEAN NOT NULL,
    CONSTRAINT step_logs_step_fk FOREIGN KEY (task_id, step_position)
        REFERENCES steps(task_id, position) ON DELETE CASCADE,
    UNIQUE (task_id, step_position)
);

CREATE TABLE IF NOT EXISTS activity_logs (
    id BIGSERIAL PRIMARY KEY,
    task_id BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    step_position INTEGER,
    event_type TEXT NOT NULL,
    worker_id TEXT,
    details JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT activity_logs_event_valid CHECK (event_type IN (
        'task_created', 'task_claimed', 'step_started', 'step_reported', 'task_done', 'task_failed'
    )),
    CONSTRAINT activity_logs_details_object CHECK (jsonb_typeof(details) = 'object')
);

CREATE INDEX IF NOT EXISTS activity_logs_recent_idx ON activity_logs (id DESC);
