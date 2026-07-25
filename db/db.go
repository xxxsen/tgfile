package db

import (
	"context"
	"fmt"

	"github.com/xxxsen/common/database"
	"github.com/xxxsen/common/database/sqlite"
)

var dbClient database.IDatabase

func InitDB(file string) error {
	return InitDBContext(context.Background(), file)
}

func InitDBContext(ctx context.Context, file string) error {
	db, err := OpenContext(ctx, file)
	if err != nil {
		return err
	}
	dbClient = db
	return nil
}

func Open(file string) (database.IDatabase, error) {
	return OpenContext(context.Background(), file)
}

func OpenContext(ctx context.Context, file string) (database.IDatabase, error) {
	db, err := sqlite.New(file)
	if err != nil {
		return nil, fmt.Errorf("open SQLite database: %w", err)
	}
	if _, err := db.ExecContext(ctx, "PRAGMA busy_timeout = 5000;"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("configure SQLite busy timeout: %w", err)
	}
	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate SQLite database: %w", err)
	}
	return db, nil
}

func GetClient() database.IDatabase {
	return dbClient
}

func Close() error {
	if dbClient == nil {
		return nil
	}
	err := dbClient.Close()
	dbClient = nil
	if err != nil {
		return fmt.Errorf("close SQLite database: %w", err)
	}
	return nil
}
