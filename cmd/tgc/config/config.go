package config

import (
	"encoding/json"
	"fmt"
	"os"
)

type Config struct {
	Schema    string `json:"schema"`
	Host      string `json:"host"`
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
	Thread    int    `json:"thread"`
	LogLevel  string `json:"log_level"`
}

func Parse(f string) (*Config, error) {
	raw, err := os.ReadFile(f)
	if err != nil {
		return nil, fmt.Errorf("read file:%w", err)
	}
	c := &Config{
		Schema:   "https",
		Thread:   5,
		LogLevel: "info",
	}
	if err := json.Unmarshal(raw, c); err != nil {
		return nil, fmt.Errorf("unmarshal file:%w", err)
	}
	return c, nil
}
