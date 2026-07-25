ALTER TABLE tg_file_tab RENAME TO tg_file_tab_migration_0005;

CREATE TABLE tg_file_tab (
    id INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT,
    file_id INTEGER NOT NULL,
    file_size INTEGER NOT NULL,
    file_part_count INTEGER NOT NULL,
    file_state INTEGER NOT NULL,
    ctime INTEGER NOT NULL,
    mtime INTEGER NOT NULL,
    extinfo TEXT NOT NULL DEFAULT '{}',
    UNIQUE (file_id)
);

INSERT INTO tg_file_tab (
    id,
    file_id,
    file_size,
    file_part_count,
    file_state,
    ctime,
    mtime,
    extinfo
)
SELECT
    id,
    file_id,
    file_size,
    file_part_count,
    file_state,
    ctime,
    mtime,
    extinfo
FROM tg_file_tab_migration_0005;

DROP TABLE tg_file_tab_migration_0005;

ALTER TABLE tg_file_part_tab RENAME TO tg_file_part_tab_migration_0005;

CREATE TABLE tg_file_part_tab (
    id INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT,
    file_id INTEGER NOT NULL,
    file_part_id INTEGER NOT NULL,
    file_key TEXT NOT NULL,
    ctime INTEGER NOT NULL,
    mtime INTEGER NOT NULL,
    file_part_md5 TEXT NOT NULL DEFAULT '',
    UNIQUE (file_id, file_part_id)
);

INSERT INTO tg_file_part_tab (
    id,
    file_id,
    file_part_id,
    file_key,
    ctime,
    mtime,
    file_part_md5
)
SELECT
    id,
    file_id,
    file_part_id,
    file_key,
    ctime,
    mtime,
    file_part_md5
FROM tg_file_part_tab_migration_0005;

DROP TABLE tg_file_part_tab_migration_0005;

ALTER TABLE tg_file_mapping_tab RENAME TO tg_file_mapping_tab_migration_0005;

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

INSERT INTO tg_file_mapping_tab (
    id,
    entry_id,
    parent_entry_id,
    ref_data,
    file_kind,
    ctime,
    mtime,
    file_size,
    file_mode,
    file_name
)
SELECT
    id,
    entry_id,
    parent_entry_id,
    ref_data,
    file_kind,
    ctime,
    mtime,
    file_size,
    file_mode,
    file_name
FROM tg_file_mapping_tab_migration_0005;

DROP TABLE tg_file_mapping_tab_migration_0005;

CREATE INDEX idx_entry_id ON tg_file_mapping_tab (entry_id);
