package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func TestSafeLogFieldsExcludeSecrets(t *testing.T) {
	const token = "TOKEN_SENTINEL_DO_NOT_LOG"
	const password = "PASSWORD_SENTINEL_DO_NOT_LOG"
	value := &Config{
		Bind:    ":9901",
		DBFile:  "/data/data.db",
		BotKind: "telegram",
		BotInfo: BotConfig{Chatid: 1, Token: token},
		UserInfo: map[string]string{
			"user": password,
		},
		S3: S3Config{
			Enable:  true,
			Buckets: []S3BucketConfig{{Name: "bucket", ACL: "private"}},
		},
	}

	var output bytes.Buffer
	encoderConfig := zap.NewProductionEncoderConfig()
	logger := zap.New(zapcore.NewCore(
		zapcore.NewJSONEncoder(encoderConfig),
		zapcore.AddSync(&output),
		zap.DebugLevel,
	))
	logger.Info("config", value.SafeLogFields()...)

	logged := output.String()
	require.NotContains(t, logged, token)
	require.NotContains(t, logged, password)
	require.NotContains(t, logged, "bot_config")
	require.NotContains(t, logged, "user_info")
	require.Contains(t, logged, `"user_count":1`)
	for _, field := range value.SafeLogFields() {
		require.False(t, strings.Contains(strings.ToLower(field.Key), "password"))
		require.False(t, strings.Contains(strings.ToLower(field.Key), "token"))
		require.False(t, strings.Contains(strings.ToLower(field.Key), "secret"))
	}
}

func TestValidateS3AndTelegramConfiguration(t *testing.T) {
	valid := &Config{
		BotKind: "telegram",
		BotInfo: map[string]any{
			"chatid":                 1,
			"token":                  "secret",
			"upload_min_interval_ms": 1000,
		},
		S3: S3Config{
			Enable:        true,
			MaxObjectSize: 5 * 1024 * 1024 * 1024,
			Buckets: []S3BucketConfig{
				{Name: "public-data", ACL: "public-read"},
				{Name: "private-data", ACL: "private"},
			},
		},
	}
	require.NoError(t, valid.Validate())
	require.Equal(t, 24, valid.S3.MultipartExpireHours)

	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{
			name: "missing buckets",
			mutate: func(config *Config) {
				config.S3.Buckets = nil
			},
		},
		{
			name: "invalid ACL",
			mutate: func(config *Config) {
				config.S3.Buckets[0].ACL = "PUBLIC"
			},
		},
		{
			name: "duplicate bucket",
			mutate: func(config *Config) {
				config.S3.Buckets[1].Name = config.S3.Buckets[0].Name
			},
		},
		{
			name: "reserved bucket",
			mutate: func(config *Config) {
				config.S3.Buckets[0].Name = "file"
			},
		},
		{
			name: "short upload interval",
			mutate: func(config *Config) {
				config.BotInfo.(map[string]any)["upload_min_interval_ms"] = 999
			},
		},
		{
			name: "object size beyond part limit",
			mutate: func(config *Config) {
				config.S3.MaxObjectSize = maxFilePartCount*telegramBlockSize + 1
			},
		},
		{
			name: "multipart expiry beyond limit",
			mutate: func(config *Config) {
				config.S3.MultipartExpireHours = 25
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			copyConfig := *valid
			copyConfig.S3.Buckets = append([]S3BucketConfig(nil), valid.S3.Buckets...)
			botConfig := make(map[string]any)
			for key, value := range valid.BotInfo.(map[string]any) {
				botConfig[key] = value
			}
			copyConfig.BotInfo = botConfig
			test.mutate(&copyConfig)
			require.ErrorIs(t, copyConfig.Validate(), errInvalidConfig)
		})
	}
}

func TestLegacyBucketFieldIsNotAccepted(t *testing.T) {
	configFile := filepath.Join(t.TempDir(), "config.json")
	require.NoError(t, os.WriteFile(configFile, []byte(`{
		"bot_kind":"telegram",
		"bot_config":{"chatid":1,"token":"secret"},
		"s3":{"enable":true,"bucket":["legacy"]}
	}`), 0o600))
	parsed, err := Parse(configFile)
	require.NoError(t, err)
	require.Empty(t, parsed.S3.Buckets)
	require.ErrorIs(t, parsed.Validate(), errInvalidConfig)
}

func TestValidateBackupConfiguration(t *testing.T) {
	dataDir := t.TempDir()
	value := &Config{
		BotKind:  "localfile",
		BotInfo:  map[string]any{"storage_dir": filepath.Join(dataDir, "blocks")},
		DBFile:   filepath.Join(dataDir, "data.db"),
		UserInfo: map[string]string{"operator": "secret", "reader": "secret"},
		Backup: BackupConfig{
			Enable: true,
			Users:  map[string]string{"operator": "read-write", "reader": "read"},
		},
	}
	require.NoError(t, value.Validate())
	require.Equal(t, filepath.Join(dataDir, "backup-work"), value.Backup.WorkDir)
	require.Equal(t, defaultBackupMaxArchiveBytes, value.Backup.MaxArchiveBytes)
	require.Equal(t, defaultBackupMaxExpandedBytes, value.Backup.MaxExpandedBytes)

	unknown := *value
	unknown.Backup = value.Backup
	unknown.Backup.Users = map[string]string{"missing": "read"}
	require.ErrorIs(t, unknown.Validate(), errInvalidConfig)

	empty := *value
	empty.Backup = value.Backup
	empty.Backup.Users = nil
	require.ErrorIs(t, empty.Validate(), errInvalidConfig)

	conflict := *value
	conflict.Backup = value.Backup
	conflict.Backup.WorkDir = value.DBFile
	require.ErrorIs(t, conflict.Validate(), errInvalidConfig)
}

func TestValidateAdminConfiguration(t *testing.T) {
	dataDir := t.TempDir()
	value := &Config{
		BotKind: "localfile",
		BotInfo: map[string]any{"storage_dir": filepath.Join(dataDir, "blocks")},
		DBFile:  filepath.Join(dataDir, "data.db"),
		UserInfo: map[string]string{
			"viewer": "view-secret", "operator": "write-secret",
		},
		ExternalOrigins: []string{
			"https://IMAGE.example.test:443/",
			"https://files.example.test",
		},
		S3: S3Config{MaxObjectSize: 1024},
		Admin: AdminConfig{
			Enable: true,
			Users: map[string]string{
				"viewer":   "read",
				"operator": "read-write",
			},
		},
	}
	require.NoError(t, value.Validate())
	require.Equal(
		t,
		[]string{"https://image.example.test", "https://files.example.test"},
		value.ExternalOrigins,
	)
	require.Equal(t, defaultAdminSessionIdleMinutes, value.Admin.SessionIdleMinutes)
	require.Equal(t, defaultAdminSessionMaxHours, value.Admin.SessionMaxHours)
	require.Equal(t, int64(1024), value.Admin.MaxUploadSize)

	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{
			name: "origin missing",
			mutate: func(config *Config) {
				config.ExternalOrigins = nil
			},
		},
		{
			name: "non loopback http",
			mutate: func(config *Config) {
				config.ExternalOrigins = []string{"http://image.example.test"}
			},
		},
		{
			name: "origin path",
			mutate: func(config *Config) {
				config.ExternalOrigins = []string{"https://image.example.test/admin"}
			},
		},
		{
			name: "origin without hostname",
			mutate: func(config *Config) {
				config.ExternalOrigins = []string{"https://:443"}
			},
		},
		{
			name: "origin port out of range",
			mutate: func(config *Config) {
				config.ExternalOrigins = []string{"https://image.example.test:65536"}
			},
		},
		{
			name: "duplicate canonical origin",
			mutate: func(config *Config) {
				config.ExternalOrigins = []string{
					"https://image.example.test",
					"https://IMAGE.example.test:443/",
				}
			},
		},
		{
			name: "mixed schemes",
			mutate: func(config *Config) {
				config.ExternalOrigins = []string{
					"https://image.example.test",
					"http://localhost",
				}
			},
		},
		{
			name: "too many origins",
			mutate: func(config *Config) {
				config.ExternalOrigins = make([]string, maxExternalOrigins+1)
				for index := range config.ExternalOrigins {
					config.ExternalOrigins[index] = fmt.Sprintf(
						"https://origin-%d.example.test",
						index,
					)
				}
			},
		},
		{
			name: "unknown user",
			mutate: func(config *Config) {
				config.Admin.Users = map[string]string{"missing": "read"}
			},
		},
		{
			name: "invalid role",
			mutate: func(config *Config) {
				config.Admin.Users = map[string]string{"viewer": "admin"}
			},
		},
		{
			name: "empty password",
			mutate: func(config *Config) {
				config.UserInfo = map[string]string{"viewer": ""}
				config.Admin.Users = map[string]string{"viewer": "read"}
			},
		},
		{
			name: "idle too short",
			mutate: func(config *Config) {
				config.Admin.SessionIdleMinutes = 4
			},
		},
		{
			name: "maximum not longer than idle",
			mutate: func(config *Config) {
				config.Admin.SessionIdleMinutes = 60
				config.Admin.SessionMaxHours = 1
			},
		},
		{
			name: "upload too large",
			mutate: func(config *Config) {
				config.Admin.MaxUploadSize = maxAdminUploadSize + 1
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			copyConfig := *value
			copyConfig.UserInfo = cloneTestMap(value.UserInfo)
			copyConfig.ExternalOrigins = append([]string(nil), value.ExternalOrigins...)
			copyConfig.Admin = value.Admin
			copyConfig.Admin.Users = cloneTestMap(value.Admin.Users)
			test.mutate(&copyConfig)
			require.ErrorIs(t, copyConfig.Validate(), errInvalidConfig)
		})
	}

	loopback := *value
	loopback.Admin = value.Admin
	loopback.Admin.Users = cloneTestMap(value.Admin.Users)
	loopback.ExternalOrigins = []string{"http://[::1]:80/", "http://localhost:80"}
	require.NoError(t, loopback.Validate())
	require.Equal(
		t,
		[]string{"http://[::1]", "http://localhost"},
		loopback.ExternalOrigins,
	)
}

func TestRootExternalOriginJSONRequiresList(t *testing.T) {
	var value Config
	require.NoError(t, json.Unmarshal(
		[]byte(`{"external_origin":["https://one.example","https://two.example"]}`),
		&value,
	))
	require.Equal(
		t,
		[]string{"https://one.example", "https://two.example"},
		value.ExternalOrigins,
	)

	require.Error(t, json.Unmarshal(
		[]byte(`{"external_origin":"https://one.example"}`),
		&value,
	))
}

func cloneTestMap(input map[string]string) map[string]string {
	result := make(map[string]string, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}

func TestValidateRootExternalOriginsForWebDAV(t *testing.T) {
	value := &Config{
		BotKind: "localfile",
		BotInfo: map[string]any{
			"storage_dir": t.TempDir(),
		},
		DBFile: filepath.Join(t.TempDir(), "data.db"),
		UserInfo: map[string]string{
			"editor": "secret",
		},
		ExternalOrigins: []string{"https://image.example.test/"},
		Webdav: WebdavConfig{
			Enable: true,
			Users: map[string]string{
				"editor": "read-write",
			},
		},
	}
	require.NoError(t, value.Validate())
	require.Equal(t, []string{"https://image.example.test"}, value.ExternalOrigins)

	for _, origin := range []string{
		"ftp://image.example.test",
		"https://",
		"https://user@example.test",
		"https://image.example.test/webdav",
		"https://image.example.test?source=proxy",
	} {
		copyConfig := *value
		copyConfig.ExternalOrigins = []string{origin}
		require.ErrorIs(t, copyConfig.Validate(), errInvalidConfig, origin)
	}

	withoutOrigins := *value
	withoutOrigins.ExternalOrigins = nil
	require.NoError(t, withoutOrigins.Validate())
}

func TestValidateIOCacheConfigurationAndPaths(t *testing.T) {
	root := t.TempDir()
	newConfig := func() *Config {
		return &Config{
			BotKind: "localfile",
			BotInfo: map[string]any{
				"dir": filepath.Join(root, "blocks"),
			},
			DBFile: filepath.Join(root, "data", "data.db"),
			IOCache: IOCacheConfig{
				EnableL1Cache:  true,
				L1CacheSize:    16,
				L1KeySizeLimit: 8,
				EnableL2Cache:  true,
				L2CacheSize:    64,
				L2KeySizeLimit: 16,
				L2CacheDir:     filepath.Join(root, "cache"),
			},
			Backup: BackupConfig{WorkDir: filepath.Join(root, "backup")},
			Webdav: WebdavConfig{UploadTempDir: filepath.Join(root, "spool")},
		}
	}
	require.NoError(t, newConfig().Validate())

	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "L1 zero size", mutate: func(config *Config) { config.IOCache.L1CacheSize = 0 }},
		{name: "L1 zero limit", mutate: func(config *Config) { config.IOCache.L1KeySizeLimit = 0 }},
		{name: "L1 limit over capacity", mutate: func(config *Config) { config.IOCache.L1KeySizeLimit = 17 }},
		{name: "L2 zero size", mutate: func(config *Config) { config.IOCache.L2CacheSize = 0 }},
		{name: "L2 zero limit", mutate: func(config *Config) { config.IOCache.L2KeySizeLimit = 0 }},
		{name: "L2 limit over capacity", mutate: func(config *Config) { config.IOCache.L2KeySizeLimit = 65 }},
		{name: "L2 empty directory", mutate: func(config *Config) { config.IOCache.L2CacheDir = "" }},
		{name: "database overlap", mutate: func(config *Config) { config.IOCache.L2CacheDir = filepath.Dir(config.DBFile) }},
		{name: "backup overlap", mutate: func(config *Config) { config.IOCache.L2CacheDir = config.Backup.WorkDir }},
		{name: "spool overlap", mutate: func(config *Config) { config.IOCache.L2CacheDir = config.Webdav.UploadTempDir }},
		{name: "backend overlap", mutate: func(config *Config) { config.IOCache.L2CacheDir = config.BotInfo.(map[string]any)["dir"].(string) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := newConfig()
			test.mutate(value)
			require.ErrorIs(t, value.Validate(), errInvalidConfig)
		})
	}

	for _, test := range []struct {
		name    string
		l1Limit int
		l2Limit int
	}{
		{name: "equal limits disable L2", l1Limit: 16, l2Limit: 16},
		{name: "greater L1 limit disables L2", l1Limit: 8, l2Limit: 4},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := newConfig()
			value.IOCache.L1KeySizeLimit = test.l1Limit
			value.IOCache.L2KeySizeLimit = test.l2Limit
			value.IOCache.L2CacheDir = ""
			require.NoError(t, value.Validate())
			require.False(t, value.IOCache.EnableL2Cache)
		})
	}
}

func TestValidateIOCacheNormalizesRelativeDirectoryAndIgnoresDisabledTierValues(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	value := &Config{
		BotKind: "localfile",
		BotInfo: map[string]any{
			"dir": filepath.Join(root, "blocks"),
		},
		DBFile: filepath.Join(root, "data.db"),
		IOCache: IOCacheConfig{
			EnableL1Cache:  false,
			L1CacheSize:    -1,
			L1KeySizeLimit: -1,
			EnableL2Cache:  true,
			L2CacheSize:    64,
			L2KeySizeLimit: 16,
			L2CacheDir:     "cache",
		},
		Backup: BackupConfig{WorkDir: filepath.Join(root, "backup")},
	}
	require.NoError(t, value.Validate())
	require.Equal(t, filepath.Join(root, "cache"), value.IOCache.L2CacheDir)
}

func TestValidateIOCacheRejectsOverlapThroughSymlinkAncestor(t *testing.T) {
	root := t.TempDir()
	backend := filepath.Join(root, "blocks")
	require.NoError(t, os.MkdirAll(backend, 0o700))
	alias := filepath.Join(root, "block-alias")
	if err := os.Symlink(backend, alias); err != nil {
		t.Skipf("symlink is unavailable: %v", err)
	}
	value := &Config{
		BotKind: "localfile",
		BotInfo: map[string]any{"dir": backend},
		DBFile:  filepath.Join(root, "data.db"),
		IOCache: IOCacheConfig{
			EnableL2Cache:  true,
			L2CacheSize:    64,
			L2KeySizeLimit: 16,
			L2CacheDir:     filepath.Join(alias, "cache"),
		},
		Backup: BackupConfig{WorkDir: filepath.Join(root, "backup")},
	}
	require.ErrorIs(t, value.Validate(), errInvalidConfig)
}
