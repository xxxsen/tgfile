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
		name: "init_tg_file_tab",
		sql: `
CREATE TABLE IF NOT EXISTS tg_file_tab (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    file_id     INTEGER NOT NULL,
	file_size   INTEGER NOT NULL,
    file_part_count  INTEGER NOT NULL,
    file_state INTEGER NOT NULL,
    ctime       INTEGER,
    mtime       INTEGER,
    UNIQUE (file_id)
);
		`,
	},
	{
		name: "init_tg_file_part_tab",
		sql: `
CREATE TABLE IF NOT EXISTS tg_file_part_tab (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    file_id       INTEGER NOT NULL,
    file_part_id  INTEGER NOT NULL,
    file_key TEXT NOT NULL,
    ctime         INTEGER,
    mtime         INTEGER,
    UNIQUE (file_id, file_part_id)
);
		`,
	},
	{
		name: "init_tg_file_mapping_tab",
		sql: `
CREATE TABLE IF NOT EXISTS tg_file_mapping_tab (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    entry_id      INTEGER NOT NULL,
    parent_entry_id INTEGER NOT NULL,
    ref_data      TEXT,
    file_kind     INTEGER,
    ctime         INTEGER,
    mtime         INTEGER,
    file_size     INTEGER,
    file_mode     INTEGER,
    file_name     TEXT NOT NULL,
    UNIQUE (parent_entry_id, file_name)
);
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
