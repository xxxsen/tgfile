package maintenance

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"time"

	_ "github.com/glebarez/go-sqlite" // Register the SQLite database/sql driver.
)

func openDatabase(ctx context.Context, file string, readOnly bool) (*sql.DB, error) {
	absolute, err := filepath.Abs(file)
	if err != nil {
		return nil, fmt.Errorf("resolve database path: %w", err)
	}

	dsn := &url.URL{Scheme: "file", Path: absolute}
	query := dsn.Query()
	if readOnly {
		query.Set("mode", "ro")
	} else {
		query.Set("mode", "rw")
	}
	query.Set("_pragma", "busy_timeout(5000)")
	dsn.RawQuery = query.Encode()

	database, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	database.SetConnMaxLifetime(time.Hour)

	if err := database.PingContext(ctx); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	if readOnly {
		if _, err := database.ExecContext(ctx, "PRAGMA query_only=ON;"); err != nil {
			_ = database.Close()
			return nil, fmt.Errorf("enable query-only mode: %w", err)
		}
	}
	return database, nil
}
