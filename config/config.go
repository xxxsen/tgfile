package config

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/xxxsen/common/logger"
)

type BotConfig struct { //默认的配置
	Chatid uint64 `json:"chatid"`
	Token  string `json:"token"`
}

type S3Config struct {
	Enable bool     `json:"enable"`
	Bucket []string `json:"bucket"`
}

type WebdavConfig struct {
	Enable bool   `json:"enable"`
	Root   string `json:"root"`
}

type Config struct {
	Bind         string            `json:"bind"`
	LogInfo      logger.LogConfig  `json:"log_info"`
	DBFile       string            `json:"db_file"`
	BotKind      string            `json:"bot_kind"`
	BotInfo      interface{}       `json:"bot_config"`
	UserInfo     map[string]string `json:"user_info"`
	S3           S3Config          `json:"s3"`
	RotateStream int               `json:"rotate_stream"`
	Webdav       WebdavConfig      `json:"webdav"`
}

func Parse(f string) (*Config, error) {
	raw, err := os.ReadFile(f)
	if err != nil {
		return nil, fmt.Errorf("read file:%w", err)
	}
	c := &Config{
		BotKind: "telegram",
	}
	if err := json.Unmarshal(raw, c); err != nil {
		return nil, fmt.Errorf("decode json failed, err:%w", err)
	}
	return c, nil
}
