package maintenance

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

var errDatabaseFileMissing = errors.New("config db_file is empty")

type AuditConfig struct {
	DatabaseFile   string
	S3Buckets      []AuditBucket
	BackendKind    string
	BackupWorkDir  string
	TelegramBotID  int64
	TelegramChatID int64
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
		BotConfig    struct {
			ChatID int64  `json:"chatid"`
			Token  string `json:"token"`
		} `json:"bot_config"`
		S3 struct {
			Buckets []struct {
				Name string `json:"name"`
				ACL  string `json:"acl"`
			} `json:"buckets"`
		} `json:"s3"`
		Backup struct {
			WorkDir string `json:"work_dir"`
		} `json:"backup"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	if value.DatabaseFile == "" {
		return nil, errDatabaseFileMissing
	}
	workDir := value.Backup.WorkDir
	if workDir == "" {
		workDir = filepath.Join(filepath.Dir(value.DatabaseFile), "backup-work")
	}
	if !filepath.IsAbs(workDir) {
		absolute, err := filepath.Abs(workDir)
		if err != nil {
			return nil, fmt.Errorf("resolve backup work dir: %w", err)
		}
		workDir = absolute
	}
	buckets := make([]AuditBucket, 0, len(value.S3.Buckets))
	for _, bucket := range value.S3.Buckets {
		buckets = append(buckets, AuditBucket{Name: bucket.Name, ACL: bucket.ACL})
	}
	botID := int64(0)
	if prefix, _, found := strings.Cut(value.BotConfig.Token, ":"); found {
		botID, _ = strconv.ParseInt(prefix, 10, 64)
	}
	return &AuditConfig{
		DatabaseFile:   value.DatabaseFile,
		S3Buckets:      buckets,
		BackendKind:    value.BotKind,
		BackupWorkDir:  workDir,
		TelegramBotID:  botID,
		TelegramChatID: value.BotConfig.ChatID,
	}, nil
}
