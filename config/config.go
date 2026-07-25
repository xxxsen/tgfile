package config

import (
	"encoding/json"
	"fmt"
	"os"

	"go.uber.org/zap"

	"github.com/xxxsen/common/logger"
)

type BotConfig struct { // 默认的配置
	Chatid uint64 `json:"chatid"`
	Token  string `json:"token"`
}

func (c *Config) SafeLogFields() []zap.Field {
	return []zap.Field{
		zap.String("bind", c.Bind),
		zap.String("db_file", c.DBFile),
		zap.String("bot_kind", c.BotKind),
		zap.Bool("s3_enable", c.S3.Enable),
		zap.Strings("s3_buckets", c.S3.Bucket),
		zap.Bool("webdav_enable", c.Webdav.Enable),
		zap.String("webdav_root", c.Webdav.Root),
		zap.Bool("l1_cache_enable", c.IOCache.EnableL1Cache),
		zap.Int("l1_cache_size", c.IOCache.L1CacheSize),
		zap.Bool("l2_cache_enable", c.IOCache.EnableL2Cache),
		zap.Int("l2_cache_size", c.IOCache.L2CacheSize),
		zap.Int("user_count", len(c.UserInfo)),
	}
}

type S3Config struct {
	Enable bool     `json:"enable"`
	Bucket []string `json:"bucket"`
}

type WebdavConfig struct {
	Enable bool   `json:"enable"`
	Root   string `json:"root"`
}

type IOCacheConfig struct {
	EnableL1Cache  bool   `json:"enable_l1_cache"`
	L1CacheSize    int    `json:"l1_cache_size"`
	L1KeySizeLimit int    `json:"l1_key_size_limit"`
	EnableL2Cache  bool   `json:"enable_l2_cache"`
	L2CacheSize    int    `json:"l2_cache_size"`
	L2KeySizeLimit int    `json:"l2_key_size_limit"`
	L2CacheDir     string `json:"l2_cache_dir"`
}

type Config struct {
	Bind         string            `json:"bind"`
	LogInfo      logger.LogConfig  `json:"log_info"`
	DBFile       string            `json:"db_file"`
	BotKind      string            `json:"bot_kind"`
	BotInfo      any               `json:"bot_config"`
	UserInfo     map[string]string `json:"user_info"`
	S3           S3Config          `json:"s3"`
	RotateStream int               `json:"rotate_stream"`
	Webdav       WebdavConfig      `json:"webdav"`
	IOCache      IOCacheConfig     `json:"io_cache"`
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
