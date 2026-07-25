CREATE TABLE tg_file_part_delete_state_tab (
    file_id INTEGER NOT NULL,
    file_part_id INTEGER NOT NULL,
    backend_kind TEXT NOT NULL,
    delete_ref TEXT NOT NULL,
    uploaded_at INTEGER NOT NULL,
    delete_state TEXT NOT NULL DEFAULT 'live'
        CHECK (delete_state IN (
            'live',
            'pending',
            'deleting',
            'deleted',
            'expired',
            'failed'
        )),
    attempt_count INTEGER NOT NULL DEFAULT 0,
    next_attempt_at INTEGER NOT NULL DEFAULT 0,
    lease_until INTEGER NOT NULL DEFAULT 0,
    last_attempt_at INTEGER NOT NULL DEFAULT 0,
    last_error_code TEXT NOT NULL DEFAULT '',
    deleted_at INTEGER NOT NULL DEFAULT 0,
    ctime INTEGER NOT NULL,
    mtime INTEGER NOT NULL,
    PRIMARY KEY (file_id, file_part_id)
);

CREATE INDEX idx_tg_file_part_delete_work
ON tg_file_part_delete_state_tab (delete_state, next_attempt_at);
