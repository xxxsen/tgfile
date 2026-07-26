CREATE TABLE tg_webdav_property_tab (
    entry_id INTEGER NOT NULL,
    namespace_uri TEXT NOT NULL,
    local_name TEXT NOT NULL,
    value_xml TEXT NOT NULL DEFAULT '',
    ctime INTEGER NOT NULL,
    mtime INTEGER NOT NULL,
    PRIMARY KEY (entry_id, namespace_uri, local_name),
    CHECK (local_name != '')
);

CREATE INDEX idx_tg_webdav_property_entry
ON tg_webdav_property_tab (entry_id);

CREATE TABLE tg_webdav_lock_tab (
    token TEXT NOT NULL PRIMARY KEY,
    root_path TEXT NOT NULL,
    root_entry_id INTEGER NOT NULL,
    lock_depth TEXT NOT NULL CHECK (lock_depth IN ('0', 'infinity')),
    owner_xml TEXT NOT NULL DEFAULT '',
    principal TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    lock_null INTEGER NOT NULL DEFAULT 0 CHECK (lock_null IN (0, 1)),
    CHECK (token != ''),
    CHECK (root_path != ''),
    CHECK (principal != ''),
    CHECK (expires_at > created_at)
);

CREATE UNIQUE INDEX uk_tg_webdav_lock_root
ON tg_webdav_lock_tab (root_path);

CREATE INDEX idx_tg_webdav_lock_expiry
ON tg_webdav_lock_tab (expires_at);

CREATE TABLE tg_webdav_change_tab (
    revision INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT,
    path TEXT NOT NULL,
    change_kind TEXT NOT NULL
        CHECK (change_kind IN ('created', 'updated', 'deleted')),
    changed_at INTEGER NOT NULL,
    CHECK (path != '')
);

CREATE INDEX idx_tg_webdav_change_path_revision
ON tg_webdav_change_tab (path, revision);
