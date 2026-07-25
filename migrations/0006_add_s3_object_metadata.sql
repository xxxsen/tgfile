CREATE UNIQUE INDEX uk_tg_file_mapping_entry_id
ON tg_file_mapping_tab (entry_id);

CREATE TABLE tg_s3_object_metadata_tab (
    entry_id INTEGER NOT NULL PRIMARY KEY,
    etag TEXT NOT NULL,
    checksum_sha256 TEXT NOT NULL DEFAULT '',
    request_checksum_algorithm TEXT NOT NULL DEFAULT '',
    request_checksum_value TEXT NOT NULL DEFAULT '',
    content_type TEXT NOT NULL DEFAULT '',
    cache_control TEXT NOT NULL DEFAULT '',
    content_disposition TEXT NOT NULL DEFAULT '',
    content_encoding TEXT NOT NULL DEFAULT '',
    content_language TEXT NOT NULL DEFAULT '',
    expires TEXT NOT NULL DEFAULT '',
    user_metadata TEXT NOT NULL DEFAULT '{}',
    ctime INTEGER NOT NULL,
    mtime INTEGER NOT NULL
);
