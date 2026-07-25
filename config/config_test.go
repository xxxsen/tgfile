package config

import (
	"bytes"
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
		S3: S3Config{Enable: true, Bucket: []string{"bucket"}},
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
