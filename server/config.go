package server

import (
	"time"

	"github.com/xxxsen/tgfile/authz"
	"github.com/xxxsen/tgfile/backupmgr"
	"github.com/xxxsen/tgfile/filemgr"
)

type config struct {
	s3            S3Options
	userMap       map[string]string
	authorizer    *authz.Authorizer
	webdav        WebDAVOptions
	backup        BackupOptions
	backupManager *backupmgr.Manager
	admin         AdminOptions
	fmgr          filemgr.IFileManager
}

type Option func(c *config)

type BucketACL string

const (
	BucketACLPrivate    BucketACL = "private"
	BucketACLPublicRead BucketACL = "public-read"
)

type S3BucketOptions struct {
	Name string
	ACL  BucketACL
}

type S3Options struct {
	Enabled              bool
	Buckets              []S3BucketOptions
	MaxObjectSize        int64
	MultipartExpireHours int
}

type WebDAVOptions struct {
	Enabled            bool
	Root               string
	ExternalOrigins    []string
	MaxUploadSize      int64
	UploadTempDir      string
	QuotaBytes         int64
	MaxMutationEntries int
	SyncPageSize       int
}

type BackupOptions struct {
	Enabled bool
}

type AdminOptions struct {
	Enabled            bool
	ExternalOrigins    []string
	SessionIdle        time.Duration
	SessionMaximum     time.Duration
	MaxUploadSize      int64
	MaxPathBytes       int
	MaxMutationEntries int
}

func WithS3(options S3Options) Option {
	return func(c *config) {
		c.s3 = options
	}
}

func WithUser(m map[string]string) Option {
	return func(c *config) {
		c.userMap = m
	}
}

func WithAuthorizer(authorizer *authz.Authorizer) Option {
	return func(c *config) {
		c.authorizer = authorizer
	}
}

func applyOpts(opts ...Option) *config {
	c := &config{}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

func WithEnableWebdav(v bool, root string) Option {
	return func(c *config) {
		c.webdav.Enabled = v
		c.webdav.Root = root
	}
}

func WithWebDAV(options WebDAVOptions) Option {
	return func(c *config) {
		c.webdav = options
	}
}

func WithFileManager(mgr filemgr.IFileManager) Option {
	return func(c *config) {
		c.fmgr = mgr
	}
}

func WithBackup(options BackupOptions, manager *backupmgr.Manager) Option {
	return func(c *config) {
		c.backup = options
		c.backupManager = manager
	}
}

func WithAdmin(options AdminOptions) Option {
	return func(c *config) {
		c.admin = options
	}
}
