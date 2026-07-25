package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xxxsen/tgfile/db"
	"github.com/xxxsen/tgfile/maintenance"
)

func setMaintenanceFlags(t *testing.T, mode, configFile, outputFile, migrationDirection string, isDryRun bool, key string) {
	t.Helper()
	previousMode := *maintenanceMode
	previousFile := *file
	previousOutput := *output
	previousDirection := *direction
	previousDryRun := *dryRun
	previousKey := *checkKey
	t.Cleanup(func() {
		*maintenanceMode = previousMode
		*file = previousFile
		*output = previousOutput
		*direction = previousDirection
		*dryRun = previousDryRun
		*checkKey = previousKey
	})
	*maintenanceMode = mode
	*file = configFile
	*output = outputFile
	*direction = migrationDirection
	*dryRun = isDryRun
	*checkKey = key
}

func createMaintenanceCLIConfig(t *testing.T) (string, string) {
	t.Helper()
	directory := t.TempDir()
	databaseFile := filepath.Join(directory, "data.db")
	database, err := db.Open(databaseFile)
	require.NoError(t, err)
	require.NoError(t, database.Close())
	configFile := filepath.Join(directory, "config.json")
	require.NoError(t, os.WriteFile(
		configFile,
		[]byte(fmt.Sprintf(`{"db_file":%q}`, databaseFile)),
		0o600,
	))
	return databaseFile, configFile
}

func TestAuditMaintenanceCLI(t *testing.T) {
	_, configFile := createMaintenanceCLIConfig(t)
	outputFile := filepath.Join(t.TempDir(), "audit.json")
	setMaintenanceFlags(t, "audit", configFile, outputFile, "", true, "")

	require.Zero(t, runMaintenance())
	raw, err := os.ReadFile(outputFile)
	require.NoError(t, err)
	var report maintenance.AuditReport
	require.NoError(t, json.Unmarshal(raw, &report))
	require.Equal(t, "ok", report.QuickCheck)
}

func TestMigrateDefaultPrefixMaintenanceCLI(t *testing.T) {
	databaseFile, configFile := createMaintenanceCLIConfig(t)
	database, err := sql.Open("sqlite", databaseFile)
	require.NoError(t, err)
	_, err = database.ExecContext(context.Background(), `
INSERT INTO tg_file_mapping_tab (
    entry_id, parent_entry_id, ref_data, file_kind,
    ctime, mtime, file_size, file_mode, file_name
) VALUES
    (1, 0, '', 1, 100, 200, 0, 420, '/'),
    (2, 1, '', 1, 100, 200, 0, 420, 'defauls');`)
	require.NoError(t, err)
	require.NoError(t, database.Close())

	setMaintenanceFlags(t, "migrate-default-prefix", configFile, "", maintenance.DirectionForward, true, "")
	require.Zero(t, runMaintenance())
	*dryRun = false
	require.Zero(t, runMaintenance())

	database, err = sql.Open("sqlite", databaseFile)
	require.NoError(t, err)
	defer database.Close()
	var name string
	require.NoError(
		t,
		database.QueryRowContext(
			t.Context(),
			"SELECT file_name FROM tg_file_mapping_tab WHERE entry_id = 2;",
		).Scan(&name),
	)
	require.Equal(t, "defaults", name)
}

func TestCheckKeyMaintenanceCLIExitCodes(t *testing.T) {
	setMaintenanceFlags(t, "check-key", "", "", "", true, "0123456789abcdef-file")
	require.Zero(t, runMaintenance())
	*checkKey = "a-x"
	require.Equal(t, 2, runMaintenance())
}
