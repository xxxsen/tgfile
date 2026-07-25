package server

import "github.com/xxxsen/tgfile/filemgr"

type config struct {
	s3           S3Options
	userMap      map[string]string
	webdavEnable bool
	webdavRoot   string
	fmgr         filemgr.IFileManager
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
		c.webdavEnable = v
		c.webdavRoot = root
	}
}

func WithFileManager(mgr filemgr.IFileManager) Option {
	return func(c *config) {
		c.fmgr = mgr
	}
}
