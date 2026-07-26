package ui

import (
	"embed"
	"fmt"
)

// Files contains the complete, self-hosted management UI.
//
//go:embed index.html app.js styles.css
var Files embed.FS

func Read(name string) ([]byte, error) {
	content, err := Files.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("read embedded admin asset %q: %w", name, err)
	}
	return content, nil
}
