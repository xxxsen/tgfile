package tgc

import "github.com/xxxsen/tgfile/tgc/client"

type config struct {
	Thread int
	Client client.IClient
}

type Option func(*config)

func WithClient(cli client.IClient) Option {
	return func(c *config) {
		c.Client = cli
	}
}

func WithThread(t int) Option {
	return func(c *config) {
		c.Thread = t
	}
}
