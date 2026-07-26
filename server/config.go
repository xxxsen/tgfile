package server

import "github.com/xxxsen/tgfile/filemgr"

type config struct {
	s3      S3Options
	userMap map[string]string
	webdav  WebDAVOptions
	fmgr    filemgr.IFileManager
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
	ExternalOrigin     string
	MaxUploadSize      int64
	UploadTempDir      string
	Users              map[string]string
	QuotaBytes         int64
	MaxMutationEntries int
	SyncPageSize       int
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
