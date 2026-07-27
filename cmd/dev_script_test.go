package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xxxsen/tgfile/config"
)

func TestDevelopmentScriptEnablesAdminWithDefaultCredentials(t *testing.T) {
	t.Parallel()

	_, currentFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	root := filepath.Dir(filepath.Dir(currentFile))
	dataDirectory := t.TempDir()
	script := filepath.Join(root, "scripts", "dev.sh")

	command := exec.CommandContext(t.Context(), "bash", script)
	command.Env = append(
		withoutEnvironmentPrefix(os.Environ(), "TGFILE_DEV_"),
		"TGFILE_DEV_CONFIG_ONLY=1",
		"TGFILE_DEV_DATA_DIR="+dataDirectory,
		"TGFILE_DEV_PORT=19901",
	)
	output, err := command.CombinedOutput()
	require.NoError(t, err, string(output))

	developmentConfig, err := config.Parse(filepath.Join(dataDirectory, "config.json"))
	require.NoError(t, err)
	require.Equal(t, "test", developmentConfig.UserInfo["test"])
	require.Equal(t, []string{"all:write"}, developmentConfig.UserPermission["test"])
	require.True(t, developmentConfig.Admin.Enable)
	require.Equal(
		t,
		[]string{"http://localhost:19901", "http://127.0.0.1:19901"},
		developmentConfig.ExternalOrigins,
	)
	require.Equal(t, filepath.Join(dataDirectory, "backup-work"), developmentConfig.Backup.WorkDir)
}

func withoutEnvironmentPrefix(environment []string, prefix string) []string {
	filtered := make([]string, 0, len(environment))
	for _, value := range environment {
		name, _, _ := strings.Cut(value, "=")
		if !strings.HasPrefix(name, prefix) {
			filtered = append(filtered, value)
		}
	}
	return filtered
}
