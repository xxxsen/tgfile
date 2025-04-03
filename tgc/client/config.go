package client

type config struct {
	Schema    string
	Host      string
	AccessKey string
	SecretKey string
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
