package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
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
		zap.Int64("webdav_max_upload_size", c.Webdav.MaxUploadSize),
		zap.Int64("webdav_quota_bytes", c.Webdav.QuotaBytes),
		zap.Int("webdav_user_count", len(c.Webdav.Users)),
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
	Enable             bool              `json:"enable"`
	Root               string            `json:"root"`
	ExternalOrigin     string            `json:"external_origin"`
	MaxUploadSize      int64             `json:"max_upload_size"`
	UploadTempDir      string            `json:"upload_temp_dir"`
	Users              map[string]string `json:"users"`
	QuotaBytes         int64             `json:"quota_bytes"`
	MaxMutationEntries int               `json:"max_mutation_entries"`
	SyncPageSize       int               `json:"sync_page_size"`
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
	defaultWebDAVMaxUploadSize      int64 = 5 * 1024 * 1024 * 1024
	defaultWebDAVMutationEntries          = 100_000
	defaultWebDAVSyncPageSize             = 1_000
)

func (c *Config) Validate() error {
	if c == nil {
		return fmt.Errorf("%w: config is nil", errInvalidConfig)
	}
	if err := c.validateS3(); err != nil {
		return err
	}
	if err := c.validateWebDAV(); err != nil {
		return err
	}
	return c.validateBlockIO()
}

func (c *Config) validateWebDAV() error {
	if !c.Webdav.Enable {
		return nil
	}
	if err := c.validateWebDAVPathAndUpload(); err != nil {
		return err
	}
	if err := c.validateWebDAVLimits(); err != nil {
		return err
	}
	return c.validateWebDAVUsers()
}

func (c *Config) validateWebDAVPathAndUpload() error {
	if strings.TrimSpace(c.Webdav.Root) == "" {
		c.Webdav.Root = "/"
	}
	if !strings.HasPrefix(c.Webdav.Root, "/") {
		return fmt.Errorf("%w: webdav.root must be an absolute path", errInvalidConfig)
	}
	if err := c.validateWebDAVExternalOrigin(); err != nil {
		return err
	}
	if c.Webdav.MaxUploadSize == 0 {
		c.Webdav.MaxUploadSize = c.S3.MaxObjectSize
		if c.Webdav.MaxUploadSize == 0 {
			c.Webdav.MaxUploadSize = defaultWebDAVMaxUploadSize
		}
	}
	if c.Webdav.MaxUploadSize < 0 {
		return fmt.Errorf("%w: webdav.max_upload_size must be positive", errInvalidConfig)
	}
	if c.BotKind == "telegram" &&
		c.Webdav.MaxUploadSize > maxFilePartCount*telegramBlockSize {
		return fmt.Errorf(
			"%w: webdav.max_upload_size exceeds Telegram storage limit",
			errInvalidConfig,
		)
	}
	if strings.TrimSpace(c.Webdav.UploadTempDir) == "" {
		c.Webdav.UploadTempDir = filepath.Join(filepath.Dir(c.DBFile), "webdav-upload")
	}
	return nil
}

func (c *Config) validateWebDAVExternalOrigin() error {
	value := strings.TrimSpace(c.Webdav.ExternalOrigin)
	if value == "" {
		return nil
	}
	origin, err := url.Parse(value)
	if err != nil ||
		(origin.Scheme != "http" && origin.Scheme != "https") ||
		origin.Host == "" ||
		origin.User != nil ||
		(origin.Path != "" && origin.Path != "/") ||
		origin.RawQuery != "" ||
		origin.Fragment != "" {
		return fmt.Errorf(
			"%w: webdav.external_origin must be an HTTP(S) origin without a path",
			errInvalidConfig,
		)
	}
	c.Webdav.ExternalOrigin = origin.Scheme + "://" + origin.Host
	return nil
}

func (c *Config) validateWebDAVLimits() error {
	if c.Webdav.QuotaBytes < 0 {
		return fmt.Errorf("%w: webdav.quota_bytes must not be negative", errInvalidConfig)
	}
	if c.Webdav.MaxMutationEntries == 0 {
		c.Webdav.MaxMutationEntries = defaultWebDAVMutationEntries
	}
	if c.Webdav.MaxMutationEntries < 1 {
		return fmt.Errorf("%w: webdav.max_mutation_entries must be positive", errInvalidConfig)
	}
	if c.Webdav.SyncPageSize == 0 {
		c.Webdav.SyncPageSize = defaultWebDAVSyncPageSize
	}
	if c.Webdav.SyncPageSize < 1 || c.Webdav.SyncPageSize > 10_000 {
		return fmt.Errorf(
			"%w: webdav.sync_page_size must be between 1 and 10000",
			errInvalidConfig,
		)
	}
	return nil
}

func (c *Config) validateWebDAVUsers() error {
	for username, role := range c.Webdav.Users {
		if _, exists := c.UserInfo[username]; !exists {
			return fmt.Errorf(
				"%w: webdav.users references unknown user %q",
				errInvalidConfig,
				username,
			)
		}
		if role != "read" && role != "read-write" {
			return fmt.Errorf(
				"%w: webdav.users[%q] must be read or read-write",
				errInvalidConfig,
				username,
			)
		}
	}
	return nil
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
