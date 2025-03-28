package server

type config struct {
	s3Buckets []string
	userMap   map[string]string
	webdav    bool
}

type Option func(c *config)

func WithS3Buckets(bks []string) Option {
	return func(c *config) {
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

func WithEnableWebdav(v bool) Option {
	return func(c *config) {
		c.webdav = v
	}
}
