CREATE TABLE tg_file_tab (
    id INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT,
    file_id INTEGER NOT NULL,
    file_size INTEGER NOT NULL,
    file_part_count INTEGER NOT NULL,
    file_state INTEGER NOT NULL,
    ctime INTEGER NOT NULL,
    mtime INTEGER NOT NULL,
    extinfo TEXT NOT NULL,
    UNIQUE (file_id)
);

CREATE TABLE tg_file_part_tab (
    id INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT,
    file_id INTEGER NOT NULL,
    file_part_id INTEGER NOT NULL,
    file_key TEXT NOT NULL,
    file_part_md5 TEXT NOT NULL DEFAULT '',
    ctime INTEGER NOT NULL,
    mtime INTEGER NOT NULL,
    UNIQUE (file_id, file_part_id)
);

CREATE TABLE tg_file_mapping_tab (
    id INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT,
    entry_id INTEGER NOT NULL,
    parent_entry_id INTEGER NOT NULL,
    ref_data TEXT NOT NULL,
    file_kind INTEGER NOT NULL,
    ctime INTEGER NOT NULL,
    mtime INTEGER NOT NULL,
    file_size INTEGER NOT NULL,
    file_mode INTEGER NOT NULL,
    file_name TEXT NOT NULL,
    UNIQUE (parent_entry_id, file_name)
);

CREATE INDEX idx_entry_id ON tg_file_mapping_tab (entry_id);
