package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"go.uber.org/zap"

	"github.com/xxxsen/common/logger"
)

type BotConfig struct { // 默认的配置
	Chatid              int64  `json:"chatid"`
	Token               string `json:"token"`
	UploadMinIntervalMS int64  `json:"upload_min_interval_ms"`
}

func (c *Config) SafeLogFields() []zap.Field {
	return []zap.Field{
		zap.String("bind", c.Bind),
		zap.String("db_file", c.DBFile),
		zap.String("bot_kind", c.BotKind),
		zap.Bool("s3_enable", c.S3.Enable),
		zap.Strings("s3_buckets", c.S3.BucketNames()),
		zap.Int("s3_multipart_expire_hours", c.S3.MultipartExpireHours),
		zap.Bool("webdav_enable", c.Webdav.Enable),
		zap.String("webdav_root", c.Webdav.Root),
		zap.Bool("l1_cache_enable", c.IOCache.EnableL1Cache),
		zap.Int("l1_cache_size", c.IOCache.L1CacheSize),
		zap.Bool("l2_cache_enable", c.IOCache.EnableL2Cache),
		zap.Int("l2_cache_size", c.IOCache.L2CacheSize),
		zap.Int("user_count", len(c.UserInfo)),
	}
}

type S3BucketConfig struct {
	Name string `json:"name"`
	ACL  string `json:"acl"`
}

type S3Config struct {
	Enable               bool             `json:"enable"`
	Buckets              []S3BucketConfig `json:"buckets"`
	MaxObjectSize        int64            `json:"max_object_size"`
	MultipartExpireHours int              `json:"multipart_expire_hours"`
}

func (c S3Config) BucketNames() []string {
	names := make([]string, 0, len(c.Buckets))
	for _, bucket := range c.Buckets {
		names = append(names, bucket.Name)
	}
	return names
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

var (
	errInvalidConfig  = errors.New("invalid configuration")
	bucketNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	reservedBuckets   = map[string]struct{}{
		"backup": {},
		"file":   {},
		"static": {},
		"webdav": {},
	}
)

const (
	defaultTelegramUploadIntervalMS int64 = 1000
	maxFilePartCount                int64 = 100_000
	telegramBlockSize               int64 = 20 * 1024 * 1024
)

func (c *Config) Validate() error {
	if c == nil {
		return fmt.Errorf("%w: config is nil", errInvalidConfig)
	}
	if err := c.validateS3(); err != nil {
		return err
	}
	return c.validateBlockIO()
}

func (c *Config) validateS3() error {
	if c.S3.MaxObjectSize < 0 {
		return fmt.Errorf("%w: s3.max_object_size must not be negative", errInvalidConfig)
	}
	if c.S3.MultipartExpireHours == 0 {
		c.S3.MultipartExpireHours = 24
	}
	if c.S3.MultipartExpireHours < 1 || c.S3.MultipartExpireHours > 24 {
		return fmt.Errorf("%w: s3.multipart_expire_hours must be between 1 and 24", errInvalidConfig)
	}
	if c.S3.Enable && len(c.S3.Buckets) == 0 {
		return fmt.Errorf("%w: s3.buckets must contain at least one bucket when S3 is enabled", errInvalidConfig)
	}
	seen := make(map[string]struct{}, len(c.S3.Buckets))
	for index, bucket := range c.S3.Buckets {
		if !bucketNamePattern.MatchString(bucket.Name) || strings.Contains(bucket.Name, "..") {
			return fmt.Errorf("%w: s3.buckets[%d].name %q is invalid", errInvalidConfig, index, bucket.Name)
		}
		if _, reserved := reservedBuckets[bucket.Name]; reserved {
			return fmt.Errorf("%w: s3.buckets[%d].name %q is reserved", errInvalidConfig, index, bucket.Name)
		}
		if _, exists := seen[bucket.Name]; exists {
			return fmt.Errorf("%w: duplicate S3 bucket %q", errInvalidConfig, bucket.Name)
		}
		seen[bucket.Name] = struct{}{}
		if bucket.ACL != "private" && bucket.ACL != "public-read" {
			return fmt.Errorf(
				"%w: s3.buckets[%d].acl must be private or public-read",
				errInvalidConfig,
				index,
			)
		}
	}
	return nil
}

func (c *Config) validateBlockIO() error {
	if c.BotKind != "telegram" {
		return nil
	}
	raw, err := json.Marshal(c.BotInfo)
	if err != nil {
		return fmt.Errorf("%w: encode bot_config: %w", errInvalidConfig, err)
	}
	var bot BotConfig
	if err := json.Unmarshal(raw, &bot); err != nil {
		return fmt.Errorf("%w: decode Telegram bot_config: %w", errInvalidConfig, err)
	}
	if bot.Chatid == 0 {
		return fmt.Errorf("%w: bot_config.chatid must not be zero", errInvalidConfig)
	}
	if strings.TrimSpace(bot.Token) == "" {
		return fmt.Errorf("%w: bot_config.token must not be empty", errInvalidConfig)
	}
	if bot.UploadMinIntervalMS == 0 {
		bot.UploadMinIntervalMS = defaultTelegramUploadIntervalMS
	}
	if bot.UploadMinIntervalMS < defaultTelegramUploadIntervalMS {
		return fmt.Errorf(
			"%w: bot_config.upload_min_interval_ms must be at least %d",
			errInvalidConfig,
			defaultTelegramUploadIntervalMS,
		)
	}
	if c.S3.MaxObjectSize > maxFilePartCount*telegramBlockSize {
		return fmt.Errorf("%w: s3.max_object_size exceeds Telegram storage limit", errInvalidConfig)
	}
	return nil
}
