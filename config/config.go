package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/xxxsen/common/logger"
)

type BotConfig struct {
	Chatid uint64 `json:"chatid"`
	Token  string `json:"token"`
}

type DebugConfig struct {
	Enable    bool   `json:"enable"`
	BlockType string `json:"block_type"`
	BlockSize int64  `json:"block_size"`
}

type Config struct {
	Bind         string            `json:"bind"`
	LogInfo      logger.LogConfig  `json:"log_info"`
	DBFile       string            `json:"db_file"`
	BotInfo      BotConfig         `json:"bot_config"`
	UserInfo     map[string]string `json:"user_info"`
	S3Bucket     []string          `json:"s3_bucket"`
	TempDir      string            `json:"temp_dir"`
	DebugMode    DebugConfig       `json:"debug_mode"`
	RotateStream int               `json:"rotate_stream"`
}

func Parse(f string) (*Config, error) {
	raw, err := os.ReadFile(f)
	if err != nil {
		return nil, fmt.Errorf("read file:%w", err)
	}
	c := &Config{
		TempDir: filepath.Join(os.TempDir(), "tgfile-temp"),
	}
	if err := json.Unmarshal(raw, c); err != nil {
		return nil, fmt.Errorf("decode json:%w", err)
	}
	return c, nil
}
