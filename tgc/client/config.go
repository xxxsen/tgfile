package client

import "time"

type config struct {
	Schema    string
	Host      string
	AccessKey string
	SecretKey string
	Timeout   time.Duration
}

type Option func(*config)

func WithSchema(s string) Option {
	return func(c *config) {
		c.Schema = s
	}
}

func WithHost(e string) Option {
	return func(c *config) {
		c.Host = e
	}
}

func WithAuth(ak string, sk string) Option {
	return func(c *config) {
		c.AccessKey = ak
		c.SecretKey = sk
	}
}

func WithTimeout(timeout time.Duration) Option {
	return func(c *config) {
		c.Timeout = timeout
	}
}
