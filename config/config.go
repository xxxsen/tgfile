package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
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
		zap.Strings("external_origins", c.ExternalOrigins),
		zap.Bool("s3_enable", c.S3.Enable),
		zap.Strings("s3_buckets", c.S3.BucketNames()),
		zap.Int("s3_multipart_expire_hours", c.S3.MultipartExpireHours),
		zap.Bool("webdav_enable", c.Webdav.Enable),
		zap.String("webdav_root", c.Webdav.Root),
		zap.Int64("webdav_max_upload_size", c.Webdav.MaxUploadSize),
		zap.Int64("webdav_quota_bytes", c.Webdav.QuotaBytes),
		zap.Int("webdav_user_count", len(c.Webdav.Users)),
		zap.Bool("backup_enable", c.Backup.Enable),
		zap.String("backup_work_dir", c.Backup.WorkDir),
		zap.Int("backup_user_count", len(c.Backup.Users)),
		zap.Int64("backup_max_archive_bytes", c.Backup.MaxArchiveBytes),
		zap.Int64("backup_max_expanded_bytes", c.Backup.MaxExpandedBytes),
		zap.Bool("admin_enable", c.Admin.Enable),
		zap.Int("admin_user_count", len(c.Admin.Users)),
		zap.Int64("admin_max_upload_size", c.Admin.MaxUploadSize),
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

type BackupConfig struct {
	Enable                 bool              `json:"enable"`
	WorkDir                string            `json:"work_dir"`
	Users                  map[string]string `json:"users"`
	MaxArchiveBytes        int64             `json:"max_archive_bytes"`
	MaxExpandedBytes       int64             `json:"max_expanded_bytes"`
	MaxMappingCount        int               `json:"max_mapping_count"`
	MaxFileCount           int               `json:"max_file_count"`
	MaxPartCount           int               `json:"max_part_count"`
	MaxPathBytes           int               `json:"max_path_bytes"`
	ArtifactRetentionHours int               `json:"artifact_retention_hours"`
	JobRetentionDays       int               `json:"job_retention_days"`
}

type AdminConfig struct {
	Enable             bool              `json:"enable"`
	Users              map[string]string `json:"users"`
	SessionIdleMinutes int               `json:"session_idle_minutes"`
	SessionMaxHours    int               `json:"session_max_hours"`
	MaxUploadSize      int64             `json:"max_upload_size"`
}

type Config struct {
	Bind            string            `json:"bind"`
	LogInfo         logger.LogConfig  `json:"log_info"`
	DBFile          string            `json:"db_file"`
	BotKind         string            `json:"bot_kind"`
	BotInfo         any               `json:"bot_config"`
	UserInfo        map[string]string `json:"user_info"`
	ExternalOrigins []string          `json:"external_origin"`
	S3              S3Config          `json:"s3"`
	RotateStream    int               `json:"rotate_stream"`
	Webdav          WebdavConfig      `json:"webdav"`
	IOCache         IOCacheConfig     `json:"io_cache"`
	Backup          BackupConfig      `json:"backup"`
	Admin           AdminConfig       `json:"admin"`
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
	defaultBackupMaxArchiveBytes    int64 = 100 * 1024 * 1024 * 1024
	defaultBackupMaxExpandedBytes   int64 = 1024 * 1024 * 1024 * 1024
	defaultBackupMaxMappingCount          = 100_000
	defaultBackupMaxFileCount             = 100_000
	defaultBackupMaxPartCount             = 1_000_000
	defaultBackupMaxPathBytes             = 1024
	defaultArtifactRetentionHours         = 24
	defaultBackupJobRetentionDays         = 30
	defaultAdminSessionIdleMinutes        = 30
	defaultAdminSessionMaxHours           = 12
	defaultAdminMaxUploadSize       int64 = 5 * 1024 * 1024 * 1024
	maxExternalOrigins                    = 32
	maxAdminUploadSize              int64 = 10 * 1024 * 1024 * 1024 * 1024
	maxBackupArchiveBytes           int64 = 10 * 1024 * 1024 * 1024 * 1024
	maxBackupExpandedBytes          int64 = 100 * 1024 * 1024 * 1024 * 1024
)

func (c *Config) Validate() error {
	if c == nil {
		return fmt.Errorf("%w: config is nil", errInvalidConfig)
	}
	if err := c.validateExternalOrigins(); err != nil {
		return err
	}
	if err := c.validateS3(); err != nil {
		return err
	}
	if err := c.validateWebDAV(); err != nil {
		return err
	}
	if err := c.validateBackup(); err != nil {
		return err
	}
	if err := c.validateAdmin(); err != nil {
		return err
	}
	return c.validateBlockIO()
}

func (c *Config) validateAdmin() error {
	if !c.Admin.Enable {
		return nil
	}
	if err := c.validateAdminUsers(); err != nil {
		return err
	}
	if err := c.validateAdminSession(); err != nil {
		return err
	}
	return c.validateAdminUploadSize()
}

func (c *Config) validateAdminUsers() error {
	if len(c.Admin.Users) == 0 {
		return fmt.Errorf("%w: admin.users must not be empty when admin is enabled", errInvalidConfig)
	}
	for username, role := range c.Admin.Users {
		password, exists := c.UserInfo[username]
		if !exists || password == "" || len(password) > 4096 {
			return fmt.Errorf(
				"%w: admin.users references unknown or empty-password user %q",
				errInvalidConfig,
				username,
			)
		}
		if username == "" || len(username) > 256 ||
			strings.IndexFunc(username, isControlCharacter) >= 0 {
			return fmt.Errorf("%w: admin username %q is invalid", errInvalidConfig, username)
		}
		if role != "read" && role != "read-write" {
			return fmt.Errorf(
				"%w: admin.users[%q] must be read or read-write",
				errInvalidConfig,
				username,
			)
		}
	}
	return nil
}

func (c *Config) validateAdminSession() error {
	if c.Admin.SessionIdleMinutes == 0 {
		c.Admin.SessionIdleMinutes = defaultAdminSessionIdleMinutes
	}
	if c.Admin.SessionIdleMinutes < 5 || c.Admin.SessionIdleMinutes > 120 {
		return fmt.Errorf(
			"%w: admin.session_idle_minutes must be between 5 and 120",
			errInvalidConfig,
		)
	}
	if c.Admin.SessionMaxHours == 0 {
		c.Admin.SessionMaxHours = defaultAdminSessionMaxHours
	}
	if c.Admin.SessionMaxHours < 1 || c.Admin.SessionMaxHours > 24 ||
		c.Admin.SessionMaxHours*60 <= c.Admin.SessionIdleMinutes {
		return fmt.Errorf(
			"%w: admin.session_max_hours must be between 1 and 24 and exceed the idle timeout",
			errInvalidConfig,
		)
	}
	return nil
}

func (c *Config) validateAdminUploadSize() error {
	if c.Admin.MaxUploadSize == 0 {
		c.Admin.MaxUploadSize = c.S3.MaxObjectSize
		if c.Admin.MaxUploadSize == 0 {
			c.Admin.MaxUploadSize = defaultAdminMaxUploadSize
		}
	}
	if c.Admin.MaxUploadSize < 1 || c.Admin.MaxUploadSize > maxAdminUploadSize {
		return fmt.Errorf(
			"%w: admin.max_upload_size is outside the supported range",
			errInvalidConfig,
		)
	}
	if c.BotKind == "telegram" &&
		c.Admin.MaxUploadSize > maxFilePartCount*telegramBlockSize {
		return fmt.Errorf(
			"%w: admin.max_upload_size exceeds Telegram storage limit",
			errInvalidConfig,
		)
	}
	return nil
}

func (c *Config) validateExternalOrigins() error {
	if len(c.ExternalOrigins) == 0 {
		if c.Admin.Enable {
			return invalidExternalOriginsError()
		}
		return nil
	}
	origins, err := normalizeExternalOrigins(c.ExternalOrigins)
	if err != nil {
		return err
	}
	c.ExternalOrigins = origins
	return nil
}

func normalizeExternalOrigins(values []string) ([]string, error) {
	if len(values) == 0 || len(values) > maxExternalOrigins {
		return nil, invalidExternalOriginsError()
	}
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	scheme := ""
	for _, value := range values {
		canonical, err := normalizeExternalOrigin(value)
		if err != nil {
			return nil, err
		}
		origin, err := url.Parse(canonical)
		if err != nil {
			return nil, invalidExternalOriginsError()
		}
		if scheme == "" {
			scheme = origin.Scheme
		} else if origin.Scheme != scheme {
			return nil, fmt.Errorf(
				"%w: external_origin entries must use one common scheme",
				errInvalidConfig,
			)
		}
		if _, exists := seen[canonical]; exists {
			return nil, fmt.Errorf(
				"%w: external_origin contains duplicate origin %q",
				errInvalidConfig,
				canonical,
			)
		}
		seen[canonical] = struct{}{}
		result = append(result, canonical)
	}
	return result, nil
}

func normalizeExternalOrigin(value string) (string, error) {
	origin, err := url.Parse(strings.TrimSpace(value))
	if err != nil {
		return "", invalidExternalOriginsError()
	}
	if !validExternalOriginShape(origin) {
		return "", invalidExternalOriginsError()
	}
	hostname := strings.ToLower(origin.Hostname())
	port := canonicalExternalOriginPort(origin.Scheme, origin.Port())
	if origin.Scheme == "http" && !isLoopbackHostname(hostname) {
		return "", fmt.Errorf(
			"%w: external_origin entries must use HTTPS outside loopback",
			errInvalidConfig,
		)
	}
	host := canonicalExternalOriginHost(hostname, port)
	return origin.Scheme + "://" + host, nil
}

func validExternalOriginShape(origin *url.URL) bool {
	if origin.Scheme != "http" && origin.Scheme != "https" {
		return false
	}
	if origin.Host == "" || origin.Hostname() == "" || origin.User != nil ||
		!validExternalOriginPort(origin.Port()) {
		return false
	}
	if origin.Path != "" && origin.Path != "/" {
		return false
	}
	return origin.RawQuery == "" && origin.Fragment == ""
}

func validExternalOriginPort(port string) bool {
	if port == "" {
		return true
	}
	value, err := strconv.Atoi(port)
	return err == nil && value >= 1 && value <= 65535
}

func canonicalExternalOriginPort(scheme, port string) string {
	if scheme == "http" && port == "80" {
		return ""
	}
	if scheme == "https" && port == "443" {
		return ""
	}
	return port
}

func canonicalExternalOriginHost(hostname, port string) string {
	if port != "" {
		return net.JoinHostPort(hostname, port)
	}
	if strings.Contains(hostname, ":") {
		return "[" + hostname + "]"
	}
	return hostname
}

func invalidExternalOriginsError() error {
	return fmt.Errorf(
		"%w: external_origin must contain 1-%d HTTP(S) origins without paths",
		errInvalidConfig,
		maxExternalOrigins,
	)
}

func isLoopbackHostname(hostname string) bool {
	if strings.EqualFold(hostname, "localhost") {
		return true
	}
	address := net.ParseIP(hostname)
	return address != nil && address.IsLoopback()
}

func isControlCharacter(value rune) bool {
	return value < 0x20 || value == 0x7f
}

func (c *Config) validateBackup() error {
	if err := c.applyBackupDefaults(); err != nil {
		return err
	}
	workDir, err := c.validateBackupPath()
	if err != nil {
		return err
	}
	if err := c.validateBackupLimits(); err != nil {
		return err
	}
	if err := c.validateBackupUsers(); err != nil {
		return err
	}
	c.Backup.WorkDir = workDir
	return nil
}

func (c *Config) validateBackupPath() (string, error) {
	if !filepath.IsAbs(c.Backup.WorkDir) {
		return "", fmt.Errorf("%w: backup.work_dir must be an absolute path", errInvalidConfig)
	}
	workDir := filepath.Clean(c.Backup.WorkDir)
	if workDir == filepath.Clean(c.DBFile) ||
		(c.IOCache.L2CacheDir != "" && workDir == filepath.Clean(c.IOCache.L2CacheDir)) ||
		(c.Webdav.UploadTempDir != "" && workDir == filepath.Clean(c.Webdav.UploadTempDir)) {
		return "", fmt.Errorf("%w: backup.work_dir conflicts with another data path", errInvalidConfig)
	}
	return workDir, nil
}

func (c *Config) validateBackupLimits() error {
	limits := []struct {
		value, minimum, maximum int64
		name                    string
	}{
		{c.Backup.MaxArchiveBytes, 1, maxBackupArchiveBytes, "max_archive_bytes"},
		{
			c.Backup.MaxExpandedBytes,
			c.Backup.MaxArchiveBytes,
			maxBackupExpandedBytes,
			"max_expanded_bytes",
		},
		{int64(c.Backup.MaxMappingCount), 1, 1_000_000, "max_mapping_count"},
		{int64(c.Backup.MaxFileCount), 1, 1_000_000, "max_file_count"},
		{int64(c.Backup.MaxPartCount), 1, 10_000_000, "max_part_count"},
		{int64(c.Backup.MaxPathBytes), 1, 4096, "max_path_bytes"},
		{int64(c.Backup.ArtifactRetentionHours), 1, 168, "artifact_retention_hours"},
		{int64(c.Backup.JobRetentionDays), 1, 365, "job_retention_days"},
	}
	for _, limit := range limits {
		if limit.value >= limit.minimum && limit.value <= limit.maximum {
			continue
		}
		return fmt.Errorf(
			"%w: backup.%s is outside the supported range",
			errInvalidConfig,
			limit.name,
		)
	}
	return nil
}

func (c *Config) validateBackupUsers() error {
	if c.Backup.Enable && len(c.Backup.Users) == 0 {
		return fmt.Errorf("%w: backup.users must not be empty when backup is enabled", errInvalidConfig)
	}
	for username, role := range c.Backup.Users {
		if _, exists := c.UserInfo[username]; !exists {
			return fmt.Errorf(
				"%w: backup.users references unknown user %q",
				errInvalidConfig,
				username,
			)
		}
		if role != "read" && role != "read-write" {
			return fmt.Errorf(
				"%w: backup.users[%q] must be read or read-write",
				errInvalidConfig,
				username,
			)
		}
	}
	return nil
}

func (c *Config) applyBackupDefaults() error {
	if strings.TrimSpace(c.Backup.WorkDir) == "" {
		workDir, err := filepath.Abs(filepath.Join(filepath.Dir(c.DBFile), "backup-work"))
		if err != nil {
			return fmt.Errorf("%w: resolve backup.work_dir: %w", errInvalidConfig, err)
		}
		c.Backup.WorkDir = workDir
	}
	if c.Backup.MaxArchiveBytes == 0 {
		c.Backup.MaxArchiveBytes = defaultBackupMaxArchiveBytes
	}
	if c.Backup.MaxExpandedBytes == 0 {
		c.Backup.MaxExpandedBytes = defaultBackupMaxExpandedBytes
	}
	if c.Backup.MaxMappingCount == 0 {
		c.Backup.MaxMappingCount = defaultBackupMaxMappingCount
	}
	if c.Backup.MaxFileCount == 0 {
		c.Backup.MaxFileCount = defaultBackupMaxFileCount
	}
	if c.Backup.MaxPartCount == 0 {
		c.Backup.MaxPartCount = defaultBackupMaxPartCount
	}
	if c.Backup.MaxPathBytes == 0 {
		c.Backup.MaxPathBytes = defaultBackupMaxPathBytes
	}
	if c.Backup.ArtifactRetentionHours == 0 {
		c.Backup.ArtifactRetentionHours = defaultArtifactRetentionHours
	}
	if c.Backup.JobRetentionDays == 0 {
		c.Backup.JobRetentionDays = defaultBackupJobRetentionDays
	}
	return nil
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
