package maintenance

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

var errDatabaseFileMissing = errors.New("config db_file is empty")

func DatabaseFileFromConfig(file string) (string, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("read config: %w", err)
	}
	var value struct {
		DatabaseFile string `json:"db_file"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("decode config: %w", err)
	}
	if value.DatabaseFile == "" {
		return "", errDatabaseFileMissing
	}
	return value.DatabaseFile, nil
}
