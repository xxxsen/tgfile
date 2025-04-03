package server

type config struct {
	s3Enable     bool
	s3Buckets    []string
	userMap      map[string]string
	webdavEnable bool
	webdavRoot   string
}

type Option func(c *config)

func WithEnableS3(enable bool, bks []string) Option {
	return func(c *config) {
		c.s3Enable = enable
		c.s3Buckets = bks
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
