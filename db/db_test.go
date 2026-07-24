package db

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/xxxsen/common/database"
	"github.com/xxxsen/common/database/sqlite"
)

func TestInitDBAddsMissingFilePartMD5Column(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "legacy.db")
	legacyDB, err := sqlite.New(dbFile, func(db database.IDatabase) error {
		_, err := db.ExecContext(context.Background(), `
CREATE TABLE tg_file_part_tab (
    id INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT,
    file_id INTEGER NOT NULL,
    file_part_id INTEGER NOT NULL,
    file_key TEXT NOT NULL,
    ctime INTEGER NOT NULL,
    mtime INTEGER NOT NULL,
    UNIQUE (file_id, file_part_id)
);`)
		return err
	})
	require.NoError(t, err)
	require.NoError(t, legacyDB.Close())

	require.NoError(t, InitDB(dbFile))
	t.Cleanup(func() {
		require.NoError(t, GetClient().Close())
	})

	_, err = GetClient().ExecContext(context.Background(), `
INSERT INTO tg_file_part_tab (
    file_id, file_part_id, file_key, file_part_md5, ctime, mtime
) VALUES (1, 0, 'block-key', 'checksum', 1, 1);`)
	require.NoError(t, err)
}
