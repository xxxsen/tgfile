package config

import (
	"bytes"
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
