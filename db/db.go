package db

import (
	"context"
	"fmt"

	"github.com/xxxsen/common/database"
	"github.com/xxxsen/common/database/sqlite"
)

var (
	dbClient database.IDatabase
)

var sqllist = []struct {
	name string
	sql  string
}{
	{
		name: "change busy_timeout",
		sql:  "PRAGMA busy_timeout = 5000;",
	},
	{
		name: "init_tg_file_tab",
		sql: `
CREATE TABLE IF NOT EXISTS tg_file_tab (
    id          INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT,
    file_id     INTEGER NOT NULL,
	file_size   INTEGER NOT NULL,
    file_part_count  INTEGER NOT NULL,
    file_state INTEGER NOT NULL,
    ctime       INTEGER NOT NULL,
    mtime       INTEGER NOT NULL,
	extinfo     TEXT NOT NULL,
    UNIQUE (file_id)
);
		`,
	},
	{
		name: "init_tg_file_part_tab",
		sql: `
CREATE TABLE IF NOT EXISTS tg_file_part_tab (
    id            INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT,
    file_id       INTEGER NOT NULL,
    file_part_id  INTEGER NOT NULL,
    file_key TEXT NOT NULL,
    ctime         INTEGER NOT NULL,
    mtime         INTEGER NOT NULL,
    UNIQUE (file_id, file_part_id)
);
		`,
	},
	{
		name: "init_tg_file_mapping_tab",
		sql: `
CREATE TABLE IF NOT EXISTS tg_file_mapping_tab (
    id            INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT,
    entry_id      INTEGER NOT NULL,
    parent_entry_id INTEGER NOT NULL,
    ref_data      TEXT NOT NULL,
    file_kind     INTEGER NOT NULL,
    ctime         INTEGER NOT NULL,
    mtime         INTEGER NOT NULL,
    file_size     INTEGER NOT NULL,
    file_mode     INTEGER NOT NULL,
    file_name     TEXT NOT NULL,
    UNIQUE (parent_entry_id, file_name)
);
		`,
	},
	{
		name: "create_entryid_index_on_tg_file_mapping_tab",
		sql: `
		CREATE INDEX IF NOT EXISTS idx_entry_id ON tg_file_mapping_tab (entry_id);
		`,
	},
}

func InitDB(file string) error {
	ctx := context.Background()
	db, err := sqlite.New(file, func(db database.IDatabase) error {
		for _, item := range sqllist {
			if _, err := db.ExecContext(ctx, item.sql); err != nil {
				return fmt.Errorf("init sql failed, sql:%s, err:%w", item.name, err)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	dbClient = db
	return nil
}

func GetClient() database.IDatabase {
	return dbClient
}
