-- Exact persisted part boundaries are required for round-tripping logical
-- backups. -1 marks pre-v2 rows whose size is derived and materialized by the
-- application from the file size and configured backend block size.
ALTER TABLE tg_file_part_tab
ADD COLUMN file_part_size INTEGER NOT NULL DEFAULT -1
    CHECK (file_part_size >= -1);

CREATE TABLE tg_backup_job_tab (
    job_id TEXT NOT NULL PRIMARY KEY CHECK (length(job_id) = 64),
    job_kind TEXT NOT NULL CHECK (job_kind IN ('export', 'import')),
    owner TEXT NOT NULL CHECK (owner != ''),
    job_state TEXT NOT NULL CHECK (job_state IN (
        'receiving',
        'queued',
        'snapshotting',
        'building',
        'validating',
        'staging',
        'publishing',
        'canceling',
        'succeeded',
        'failed',
        'canceled'
    )),
    scope TEXT NOT NULL DEFAULT '/',
    dry_run INTEGER NOT NULL DEFAULT 0 CHECK (dry_run IN (0, 1)),
    conflict_policy TEXT NOT NULL DEFAULT 'fail'
        CHECK (conflict_policy IN ('fail', 'replace')),
    idempotency_key TEXT NOT NULL,
    request_fingerprint TEXT NOT NULL,
    artifact_path TEXT NOT NULL DEFAULT '',
    snapshot_path TEXT NOT NULL DEFAULT '',
    report_path TEXT NOT NULL DEFAULT '',
    artifact_size INTEGER NOT NULL DEFAULT 0 CHECK (artifact_size >= 0),
    artifact_sha256 TEXT NOT NULL DEFAULT '',
    files_total INTEGER NOT NULL DEFAULT 0 CHECK (files_total >= 0),
    files_completed INTEGER NOT NULL DEFAULT 0 CHECK (files_completed >= 0),
    parts_total INTEGER NOT NULL DEFAULT 0 CHECK (parts_total >= 0),
    parts_completed INTEGER NOT NULL DEFAULT 0 CHECK (parts_completed >= 0),
    bytes_total INTEGER NOT NULL DEFAULT 0 CHECK (bytes_total >= 0),
    bytes_completed INTEGER NOT NULL DEFAULT 0 CHECK (bytes_completed >= 0),
    mappings_created INTEGER NOT NULL DEFAULT 0 CHECK (mappings_created >= 0),
    mappings_replaced INTEGER NOT NULL DEFAULT 0 CHECK (mappings_replaced >= 0),
    files_created INTEGER NOT NULL DEFAULT 0 CHECK (files_created >= 0),
    error_code TEXT NOT NULL DEFAULT '',
    error_message TEXT NOT NULL DEFAULT '',
    cancel_requested INTEGER NOT NULL DEFAULT 0 CHECK (cancel_requested IN (0, 1)),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    completed_at INTEGER NOT NULL DEFAULT 0,
    artifact_expires_at INTEGER NOT NULL DEFAULT 0,
    UNIQUE (owner, job_kind, idempotency_key)
);

CREATE INDEX idx_tg_backup_job_work
ON tg_backup_job_tab (job_kind, job_state, created_at);

CREATE INDEX idx_tg_backup_job_retention
ON tg_backup_job_tab (completed_at, artifact_expires_at);

CREATE TABLE tg_backup_job_file_tab (
    job_id TEXT NOT NULL,
    file_ref TEXT NOT NULL CHECK (length(file_ref) = 9),
    source_file_id TEXT NOT NULL DEFAULT '',
    target_file_id INTEGER NOT NULL DEFAULT 0,
    layout_version INTEGER NOT NULL CHECK (layout_version IN (1, 2)),
    stage_state TEXT NOT NULL CHECK (stage_state IN (
        'queued',
        'uploading',
        'verifying',
        'ready',
        'failed'
    )),
    next_part_index INTEGER NOT NULL DEFAULT 0 CHECK (next_part_index >= 0),
    error_code TEXT NOT NULL DEFAULT '',
    ctime INTEGER NOT NULL,
    mtime INTEGER NOT NULL,
    PRIMARY KEY (job_id, file_ref)
);

CREATE INDEX idx_tg_backup_job_file_target
ON tg_backup_job_file_tab (target_file_id);

CREATE TABLE tg_backup_export_pin_tab (
    job_id TEXT NOT NULL,
    file_id INTEGER NOT NULL CHECK (file_id > 0),
    ctime INTEGER NOT NULL,
    PRIMARY KEY (job_id, file_id)
);

CREATE INDEX idx_tg_backup_export_pin_file
ON tg_backup_export_pin_tab (file_id);
