ALTER TABLE tg_file_tab
ADD COLUMN file_layout_version INTEGER NOT NULL DEFAULT 1
    CHECK (file_layout_version IN (1, 2));

CREATE TABLE tg_s3_file_segment_tab (
    file_id INTEGER NOT NULL,
    segment_index INTEGER NOT NULL CHECK (segment_index >= 0),
    source_file_id INTEGER NOT NULL,
    segment_size INTEGER NOT NULL CHECK (segment_size >= 0),
    ctime INTEGER NOT NULL,
    mtime INTEGER NOT NULL,
    PRIMARY KEY (file_id, segment_index),
    UNIQUE (source_file_id)
);

CREATE INDEX idx_tg_s3_file_segment_source
ON tg_s3_file_segment_tab (source_file_id);

CREATE TABLE tg_s3_multipart_upload_tab (
    upload_id TEXT NOT NULL PRIMARY KEY CHECK (length(upload_id) = 64),
    bucket_name TEXT NOT NULL,
    object_key TEXT NOT NULL CHECK (length(object_key) > 0),
    upload_state TEXT NOT NULL
        CHECK (upload_state IN ('active', 'completing', 'completed', 'aborted')),
    content_type TEXT NOT NULL DEFAULT '',
    cache_control TEXT NOT NULL DEFAULT '',
    content_disposition TEXT NOT NULL DEFAULT '',
    content_encoding TEXT NOT NULL DEFAULT '',
    content_language TEXT NOT NULL DEFAULT '',
    expires TEXT NOT NULL DEFAULT '',
    user_metadata TEXT NOT NULL DEFAULT '{}',
    completion_fingerprint TEXT NOT NULL DEFAULT '',
    result_file_id INTEGER NOT NULL DEFAULT 0,
    result_etag TEXT NOT NULL DEFAULT '',
    initiated_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    completed_at INTEGER NOT NULL DEFAULT 0,
    cleanup_at INTEGER NOT NULL DEFAULT 0,
    ctime INTEGER NOT NULL,
    mtime INTEGER NOT NULL
);

CREATE INDEX idx_tg_s3_multipart_upload_list
ON tg_s3_multipart_upload_tab (
    bucket_name,
    upload_state,
    object_key,
    upload_id
);

CREATE INDEX idx_tg_s3_multipart_upload_expire
ON tg_s3_multipart_upload_tab (upload_state, expires_at);

CREATE INDEX idx_tg_s3_multipart_upload_cleanup
ON tg_s3_multipart_upload_tab (upload_state, cleanup_at);

CREATE TABLE tg_s3_multipart_part_tab (
    upload_id TEXT NOT NULL,
    part_number INTEGER NOT NULL CHECK (part_number BETWEEN 1 AND 10000),
    part_state TEXT NOT NULL
        CHECK (part_state IN ('active', 'selected', 'discarded')),
    file_id INTEGER NOT NULL,
    part_size INTEGER NOT NULL CHECK (
        part_size >= 0 AND part_size <= 5368709120
    ),
    part_etag TEXT NOT NULL CHECK (length(part_etag) = 32),
    uploaded_at INTEGER NOT NULL,
    ctime INTEGER NOT NULL,
    mtime INTEGER NOT NULL,
    PRIMARY KEY (upload_id, part_number),
    UNIQUE (file_id)
);

CREATE INDEX idx_tg_s3_multipart_part_file
ON tg_s3_multipart_part_tab (file_id);
