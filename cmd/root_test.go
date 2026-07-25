package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xxxsen/tgfile/db"
	"github.com/xxxsen/tgfile/maintenance"
)

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

func executeForTest(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := execute(t.Context(), args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestAuditCommand(t *testing.T) {
	_, configFile := createMaintenanceCLIConfig(t)
	outputFile := filepath.Join(t.TempDir(), "audit.json")

	code, _, stderr := executeForTest(
		t,
		"audit",
		"--config="+configFile,
		"--output="+outputFile,
	)
	require.Zero(t, code, stderr)
	raw, err := os.ReadFile(outputFile)
	require.NoError(t, err)
	var report maintenance.AuditReport
	require.NoError(t, json.Unmarshal(raw, &report))
	require.Equal(t, "ok", report.QuickCheck)
}

func TestCheckKeyCommandExitCodes(t *testing.T) {
	code, stdout, stderr := executeForTest(
		t,
		"check-key",
		"--key=0123456789abcdef-file",
	)
	require.Zero(t, code, stderr)
	require.Equal(t, "/defaults/01/0123456789abcdef-file\n", stdout)

	code, _, stderr = executeForTest(t, "check-key", "--key=a-x")
	require.Equal(t, 2, code)
	require.Contains(t, stderr, "validate file key")
}

func TestCommandFlagsAreScoped(t *testing.T) {
	code, _, stderr := executeForTest(t, "audit", "--key=value")
	require.Equal(t, 2, code)
	require.Contains(t, stderr, "unknown flag")

	code, _, stderr = executeForTest(t, "audit", "unexpected")
	require.Equal(t, 2, code)
	require.Contains(t, stderr, "unexpected positional arguments")
}

func TestRootCommandRequiresSubcommand(t *testing.T) {
	code, _, stderr := executeForTest(t)
	require.Equal(t, 2, code)
	require.Contains(t, stderr, "subcommand is required")
}

func TestRootCommandHelpListsOnlyBusinessCommands(t *testing.T) {
	code, stdout, stderr := executeForTest(t, "--help")
	require.Zero(t, code, stderr)
	for _, command := range []string{"serve", "audit", "check-key"} {
		require.Contains(t, stdout, command)
	}
	require.NotContains(t, stdout, "migrate-default-prefix")
	require.NotContains(t, stdout, "completion")
}

func TestRetiredMigrationCommandIsUnavailable(t *testing.T) {
	code, _, stderr := executeForTest(t, "migrate-default-prefix", "--help")
	require.Equal(t, 1, code)
	require.Contains(t, stderr, `unknown command "migrate-default-prefix"`)
}
