package maintenance

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

var errDatabaseFileMissing = errors.New("config db_file is empty")

type AuditConfig struct {
	DatabaseFile string
	S3Buckets    []AuditBucket
	BackendKind  string
}

func DatabaseFileFromConfig(file string) (string, error) {
	config, err := ReadAuditConfig(file)
	if err != nil {
		return "", err
	}
	return config.DatabaseFile, nil
}

func ReadAuditConfig(file string) (*AuditConfig, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var value struct {
		DatabaseFile string `json:"db_file"`
		BotKind      string `json:"bot_kind"`
		S3           struct {
			Buckets []struct {
				Name string `json:"name"`
				ACL  string `json:"acl"`
			} `json:"buckets"`
		} `json:"s3"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	if value.DatabaseFile == "" {
		return nil, errDatabaseFileMissing
	}
	buckets := make([]AuditBucket, 0, len(value.S3.Buckets))
	for _, bucket := range value.S3.Buckets {
		buckets = append(buckets, AuditBucket{Name: bucket.Name, ACL: bucket.ACL})
	}
	return &AuditConfig{
		DatabaseFile: value.DatabaseFile,
		S3Buckets:    buckets,
		BackendKind:  value.BotKind,
	}, nil
}
