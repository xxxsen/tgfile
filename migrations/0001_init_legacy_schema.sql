CREATE TABLE tg_file_tab (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    file_id INTEGER NOT NULL,
    file_size INTEGER NOT NULL,
    file_part_count INTEGER NOT NULL,
    file_state INTEGER NOT NULL,
    ctime INTEGER,
    mtime INTEGER,
    UNIQUE (file_id)
);

CREATE TABLE tg_file_part_tab (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    file_id INTEGER NOT NULL,
    file_part_id INTEGER NOT NULL,
    file_key TEXT NOT NULL,
    ctime INTEGER,
    mtime INTEGER,
    UNIQUE (file_id, file_part_id)
);

CREATE TABLE tg_file_mapping_tab (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    entry_id INTEGER NOT NULL,
    parent_entry_id INTEGER NOT NULL,
    ref_data TEXT,
    file_kind INTEGER,
    ctime INTEGER,
    mtime INTEGER,
    file_size INTEGER,
    file_mode INTEGER,
    file_name TEXT NOT NULL,
    UNIQUE (parent_entry_id, file_name)
);
