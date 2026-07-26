CREATE INDEX idx_tg_file_mapping_admin_page
ON tg_file_mapping_tab (
    parent_entry_id,
    file_kind,
    file_name COLLATE BINARY,
    entry_id
);

CREATE INDEX idx_tg_backup_job_admin_page
ON tg_backup_job_tab (created_at DESC, job_id DESC);

CREATE INDEX idx_tg_backup_job_admin_owner_page
ON tg_backup_job_tab (owner, created_at DESC, job_id DESC);
