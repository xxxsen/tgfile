package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xxxsen/common/database"
	"github.com/xxxsen/common/database/sqlite"

	"github.com/xxxsen/tgfile/migrations"
)

const legacyBaselineMaxVersion = 5

var (
	migrationFSMu sync.RWMutex
	migrationFS   fs.FS = migrations.FS

	errMigrationNameFormat       = errors.New("migration filename must match NNNN_name.sql")
	errDuplicateMigrationVersion = errors.New("duplicate migration version")
	errMigrationChanged          = errors.New("applied migration differs from embedded migration")
	errMigrationLedgerGap        = errors.New("migration ledger contains a version gap")
	errUnrecognizedLegacySchema  = errors.New("database without migration ledger has an unrecognized schema")
	errSchemaDrift               = errors.New("database schema differs from registered migrations")
	errNoMigrations              = errors.New("database has no embedded migrations")
	errMigrationsNotRegistered   = errors.New("database migrations are not registered")
	errEmptyMigration            = errors.New("migration is empty")
	errMissingQueryResult        = errors.New("database query returned no result")
	errLegacyProfileVersion      = errors.New("legacy schema profile references a missing migration version")

	migrationNamePattern = regexp.MustCompile(`^([0-9]{4})_([a-z0-9]+(?:_[a-z0-9]+)*)\.sql$`)
	legacySchemaProfiles = []legacySchemaProfile{
		{path: "legacy/strict_current_schema.sql", baselineVersion: 4},
	}
)

type migrationFile struct {
	version  int
	filename string
	checksum string
	body     []byte
}

type migrationRecord struct {
	filename string
	checksum string
}

type migrationPlan struct {
	files       []migrationFile
	baseline    []migrationFile
	current     []migrationFile
	pending     []migrationFile
	needsLedger bool
}

type legacySchemaProfile struct {
	path            string
	baselineVersion int
}

func setMigrationFS(source fs.FS) {
	migrationFSMu.Lock()
	migrationFS = source
	migrationFSMu.Unlock()
}

func getMigrationFS() fs.FS {
	migrationFSMu.RLock()
	defer migrationFSMu.RUnlock()
	return migrationFS
}

func migrate(ctx context.Context, db database.IDatabase) error {
	files, err := listMigrationFiles(getMigrationFS())
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return errNoMigrations
	}
	plan, err := planMigrations(ctx, db, files)
	if err != nil {
		return err
	}
	if err := validateCurrentSchemaFingerprint(ctx, db, plan.current); err != nil {
		return err
	}
	if plan.needsLedger {
		if err := initializeMigrationLedger(ctx, db, plan.baseline); err != nil {
			return err
		}
	}
	for _, file := range plan.pending {
		if err := applyMigration(ctx, db, file); err != nil {
			return err
		}
	}
	if len(plan.pending) == 0 {
		return nil
	}
	return validateSchemaFingerprint(ctx, db, plan.files)
}

func planMigrations(
	ctx context.Context,
	db database.IDatabase,
	files []migrationFile,
) (*migrationPlan, error) {
	hasLedger, err := tableExists(ctx, db, "schema_migrations")
	if err != nil {
		return nil, err
	}
	if !hasLedger {
		baseline, err := recognizeLegacyBaseline(ctx, db, files)
		if err != nil {
			return nil, err
		}
		return &migrationPlan{
			files:       files,
			baseline:    baseline,
			current:     baseline,
			pending:     files[len(baseline):],
			needsLedger: true,
		}, nil
	}

	applied, err := readAppliedMigrations(ctx, db)
	if err != nil {
		return nil, err
	}
	pending := make([]migrationFile, 0, len(files))
	foundGap := false
	for _, file := range files {
		record, ok := applied[file.version]
		if !ok {
			foundGap = true
			pending = append(pending, file)
			continue
		}
		if foundGap {
			return nil, fmt.Errorf(
				"migration %04d is applied after a missing earlier version: %w",
				file.version,
				errMigrationLedgerGap,
			)
		}
		if record.filename != file.filename || record.checksum != file.checksum {
			return nil, fmt.Errorf(
				"migration %04d: recorded %q/%s, embedded %q/%s: %w",
				file.version,
				record.filename,
				record.checksum,
				file.filename,
				file.checksum,
				errMigrationChanged,
			)
		}
		delete(applied, file.version)
	}
	if len(applied) != 0 {
		versions := make([]int, 0, len(applied))
		for version := range applied {
			versions = append(versions, version)
		}
		sort.Ints(versions)
		return nil, fmt.Errorf(
			"applied migration %04d is missing from the binary: %w",
			versions[0],
			errMigrationChanged,
		)
	}
	current := files[:len(files)-len(pending)]
	return &migrationPlan{files: files, current: current, pending: pending}, nil
}

func validateSchemaFingerprint(
	ctx context.Context,
	db database.IQueryer,
	files []migrationFile,
) error {
	expected, err := expectedSchemaFingerprint(ctx, files)
	if err != nil {
		return err
	}
	actual, err := readSchemaFingerprint(ctx, db)
	if err != nil {
		return err
	}
	if !slices.Equal(actual, expected) {
		return newSchemaDriftError(expected, actual)
	}
	return nil
}

func validateCurrentSchemaFingerprint(
	ctx context.Context,
	db database.IQueryer,
	files []migrationFile,
) error {
	expected, err := expectedSchemaFingerprint(ctx, files)
	if err != nil {
		return err
	}
	actual, err := readSchemaFingerprint(ctx, db)
	if err != nil {
		return err
	}
	if slices.Equal(actual, expected) {
		return nil
	}
	if len(files) == 0 {
		return newSchemaDriftError(expected, actual)
	}
	currentVersion := files[len(files)-1].version
	for _, profile := range legacySchemaProfiles {
		if profile.baselineVersion != currentVersion {
			continue
		}
		profileFingerprint, err := legacyProfileFingerprint(ctx, profile)
		if err != nil {
			return err
		}
		if slices.Equal(actual, profileFingerprint) {
			return nil
		}
	}
	return newSchemaDriftError(expected, actual)
}

func newSchemaDriftError(expected, actual []string) error {
	return fmt.Errorf(
		"%w: expected [%s], actual [%s]",
		errSchemaDrift,
		strings.Join(expected, ", "),
		strings.Join(actual, ", "),
	)
}

func recognizeLegacyBaseline(
	ctx context.Context,
	db database.IDatabase,
	files []migrationFile,
) ([]migrationFile, error) {
	actual, err := readSchemaFingerprint(ctx, db)
	if err != nil {
		return nil, err
	}
	if len(actual) == 0 {
		return nil, nil
	}

	var baseline []migrationFile
	for index, file := range files {
		if file.version > legacyBaselineMaxVersion {
			break
		}
		expected, err := expectedSchemaFingerprint(ctx, files[:index+1])
		if err != nil {
			return nil, err
		}
		if slices.Equal(actual, expected) {
			baseline = files[:index+1]
		}
	}
	if len(baseline) != 0 {
		return baseline, nil
	}
	for _, profile := range legacySchemaProfiles {
		expected, err := legacyProfileFingerprint(ctx, profile)
		if err != nil {
			return nil, err
		}
		if !slices.Equal(actual, expected) {
			continue
		}
		baseline, err := migrationPrefixThroughVersion(files, profile.baselineVersion)
		if err != nil {
			return nil, err
		}
		return baseline, nil
	}
	return nil, fmt.Errorf("%w: [%s]", errUnrecognizedLegacySchema, strings.Join(actual, ", "))
}

func legacyProfileFingerprint(
	ctx context.Context,
	profile legacySchemaProfile,
) ([]string, error) {
	body, err := fs.ReadFile(migrations.FS, profile.path)
	if err != nil {
		return nil, fmt.Errorf("read legacy schema profile %s: %w", profile.path, err)
	}
	return expectedSchemaFingerprint(ctx, []migrationFile{{
		filename: profile.path,
		body:     body,
	}})
}

func migrationPrefixThroughVersion(
	files []migrationFile,
	version int,
) ([]migrationFile, error) {
	for index, file := range files {
		if file.version == version {
			return files[:index+1], nil
		}
	}
	return nil, fmt.Errorf(
		"legacy baseline version %04d: %w",
		version,
		errLegacyProfileVersion,
	)
}

func initializeMigrationLedger(
	ctx context.Context,
	db database.IDatabase,
	baseline []migrationFile,
) error {
	err := db.OnTransation(ctx, func(ctx context.Context, tx database.IQueryExecer) error {
		if _, err := tx.ExecContext(ctx, `
CREATE TABLE schema_migrations (
    version INTEGER PRIMARY KEY,
    filename TEXT NOT NULL UNIQUE,
    checksum TEXT NOT NULL,
    applied_at INTEGER NOT NULL
);`); err != nil {
			return fmt.Errorf("create migration ledger: %w", err)
		}
		appliedAt := time.Now().UTC().UnixMilli()
		for _, file := range baseline {
			if _, err := tx.ExecContext(
				ctx,
				`INSERT INTO schema_migrations(version, filename, checksum, applied_at)
				 VALUES (?, ?, ?, ?)`,
				file.version,
				file.filename,
				file.checksum,
				appliedAt,
			); err != nil {
				return fmt.Errorf("record legacy migration %s: %w", file.filename, err)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("initialize migration ledger: %w", err)
	}
	return nil
}

func applyMigration(ctx context.Context, db database.IDatabase, file migrationFile) error {
	err := db.OnTransation(ctx, func(ctx context.Context, tx database.IQueryExecer) error {
		if _, err := tx.ExecContext(ctx, string(file.body)); err != nil {
			return fmt.Errorf("execute migration %s: %w", file.filename, err)
		}
		if _, err := tx.ExecContext(
			ctx,
			`INSERT INTO schema_migrations(version, filename, checksum, applied_at)
			 VALUES (?, ?, ?, ?)`,
			file.version,
			file.filename,
			file.checksum,
			time.Now().UTC().UnixMilli(),
		); err != nil {
			return fmt.Errorf("record migration %s: %w", file.filename, err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("apply migration %s: %w", file.filename, err)
	}
	return nil
}

func listMigrationFiles(source fs.FS) ([]migrationFile, error) {
	if source == nil {
		return nil, errMigrationsNotRegistered
	}
	entries, err := fs.ReadDir(source, ".")
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}
	files := make([]migrationFile, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		matches := migrationNamePattern.FindStringSubmatch(entry.Name())
		if matches == nil {
			return nil, fmt.Errorf("migration %q: %w", entry.Name(), errMigrationNameFormat)
		}
		version, err := strconv.Atoi(matches[1])
		if err != nil {
			return nil, fmt.Errorf("parse migration %q: %w", entry.Name(), err)
		}
		body, err := fs.ReadFile(source, entry.Name())
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", entry.Name(), err)
		}
		if len(strings.TrimSpace(string(body))) == 0 {
			return nil, fmt.Errorf("migration %q: %w", entry.Name(), errEmptyMigration)
		}
		sum := sha256.Sum256(body)
		files = append(files, migrationFile{
			version:  version,
			filename: entry.Name(),
			checksum: fmt.Sprintf("%x", sum[:]),
			body:     body,
		})
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].version < files[j].version
	})
	for index := 1; index < len(files); index++ {
		if files[index].version == files[index-1].version {
			return nil, fmt.Errorf(
				"migration version %04d is duplicated: %w",
				files[index].version,
				errDuplicateMigrationVersion,
			)
		}
	}
	return files, nil
}

func readAppliedMigrations(
	ctx context.Context,
	db database.IQueryer,
) (map[int]migrationRecord, error) {
	rows, err := db.QueryContext(
		ctx,
		`SELECT version, filename, checksum FROM schema_migrations ORDER BY version`,
	)
	if err != nil {
		return nil, fmt.Errorf("read migration ledger: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()
	result := make(map[int]migrationRecord)
	for rows.Next() {
		var version int
		var record migrationRecord
		if err := rows.Scan(&version, &record.filename, &record.checksum); err != nil {
			return nil, fmt.Errorf("scan migration ledger: %w", err)
		}
		result[version] = record
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate migration ledger: %w", err)
	}
	return result, nil
}

func tableExists(ctx context.Context, db database.IQueryer, name string) (bool, error) {
	var count int
	rows, err := db.QueryContext(
		ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`,
		name,
	)
	if err != nil {
		return false, fmt.Errorf("inspect table %s: %w", name, err)
	}
	defer func() {
		_ = rows.Close()
	}()
	if !rows.Next() {
		return false, fmt.Errorf("inspect table %s: %w", name, errMissingQueryResult)
	}
	if err := rows.Scan(&count); err != nil {
		return false, fmt.Errorf("scan table %s existence: %w", name, err)
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("read table %s existence: %w", name, err)
	}
	return count == 1, nil
}

func expectedSchemaFingerprint(
	ctx context.Context,
	files []migrationFile,
) ([]string, error) {
	expectedDB, err := sqlite.New(":memory:", func(db database.IDatabase) error {
		for _, file := range files {
			if _, err := db.ExecContext(ctx, string(file.body)); err != nil {
				return fmt.Errorf("build expected schema from %s: %w", file.filename, err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("open expected schema database: %w", err)
	}
	defer func() {
		_ = expectedDB.Close()
	}()
	return readSchemaFingerprint(ctx, expectedDB)
}

func readSchemaFingerprint(ctx context.Context, db database.IQueryer) ([]string, error) {
	tables, err := listSchemaTables(ctx, db)
	if err != nil {
		return nil, err
	}

	fingerprint := make([]string, 0)
	for _, table := range tables {
		fingerprint = append(fingerprint, "table|"+table)
		columns, err := readColumnFingerprint(ctx, db, table)
		if err != nil {
			return nil, err
		}
		fingerprint = append(fingerprint, columns...)
		indexes, err := readIndexFingerprint(ctx, db, table)
		if err != nil {
			return nil, err
		}
		fingerprint = append(fingerprint, indexes...)
	}
	objects, err := readAuxiliarySchemaFingerprint(ctx, db)
	if err != nil {
		return nil, err
	}
	fingerprint = append(fingerprint, objects...)
	sort.Strings(fingerprint)
	return fingerprint, nil
}

func listSchemaTables(ctx context.Context, db database.IQueryer) ([]string, error) {
	rows, err := db.QueryContext(
		ctx,
		`SELECT name FROM sqlite_master
		 WHERE type = 'table'
		   AND name NOT LIKE 'sqlite_%'
		   AND name <> 'schema_migrations'
		 ORDER BY name`,
	)
	if err != nil {
		return nil, fmt.Errorf("list schema tables: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()
	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			return nil, fmt.Errorf("scan schema table: %w", err)
		}
		tables = append(tables, table)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate schema tables: %w", err)
	}
	return tables, nil
}

func readColumnFingerprint(
	ctx context.Context,
	db database.IQueryer,
	table string,
) ([]string, error) {
	rows, err := db.QueryContext(ctx, "PRAGMA table_info("+quoteIdentifier(table)+")")
	if err != nil {
		return nil, fmt.Errorf("inspect columns for %s: %w", table, err)
	}
	defer func() {
		_ = rows.Close()
	}()
	var result []string
	for rows.Next() {
		var (
			columnID    int
			name        string
			columnType  string
			notNull     int
			defaultText sql.NullString
			primaryKey  int
		)
		if err := rows.Scan(
			&columnID,
			&name,
			&columnType,
			&notNull,
			&defaultText,
			&primaryKey,
		); err != nil {
			return nil, fmt.Errorf("scan columns for %s: %w", table, err)
		}
		result = append(result, fmt.Sprintf(
			"column|%s|%s|%s|%d|%s|%d",
			table,
			name,
			strings.ToUpper(strings.Join(strings.Fields(columnType), " ")),
			notNull,
			normalizeDefaultValue(defaultText),
			primaryKey,
		))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate columns for %s: %w", table, err)
	}
	sort.Strings(result)
	return result, nil
}

func readIndexFingerprint(
	ctx context.Context,
	db database.IQueryer,
	table string,
) ([]string, error) {
	indexes, err := listIndexes(ctx, db, table)
	if err != nil {
		return nil, err
	}
	result := make([]string, 0, len(indexes))
	for _, index := range indexes {
		indexName := ""
		if index.origin == "c" {
			indexName = index.name
		}
		columns, err := readIndexColumns(ctx, db, index.name)
		if err != nil {
			return nil, err
		}
		result = append(result, fmt.Sprintf(
			"index|%s|%s|%d|%s|%d|%s",
			table,
			indexName,
			index.unique,
			index.origin,
			index.partial,
			strings.Join(columns, ","),
		))
	}
	sort.Strings(result)
	return result, nil
}

type indexInfo struct {
	name    string
	unique  int
	origin  string
	partial int
}

func listIndexes(
	ctx context.Context,
	db database.IQueryer,
	table string,
) ([]indexInfo, error) {
	rows, err := db.QueryContext(ctx, "PRAGMA index_list("+quoteIdentifier(table)+")")
	if err != nil {
		return nil, fmt.Errorf("inspect indexes for %s: %w", table, err)
	}
	defer func() {
		_ = rows.Close()
	}()
	var indexes []indexInfo
	for rows.Next() {
		var sequence int
		var index indexInfo
		if err := rows.Scan(
			&sequence,
			&index.name,
			&index.unique,
			&index.origin,
			&index.partial,
		); err != nil {
			return nil, fmt.Errorf("scan indexes for %s: %w", table, err)
		}
		indexes = append(indexes, index)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate indexes for %s: %w", table, err)
	}
	return indexes, nil
}

func readIndexColumns(
	ctx context.Context,
	db database.IQueryer,
	index string,
) ([]string, error) {
	rows, err := db.QueryContext(ctx, "PRAGMA index_xinfo("+quoteIdentifier(index)+")")
	if err != nil {
		return nil, fmt.Errorf("inspect index %s: %w", index, err)
	}
	defer func() {
		_ = rows.Close()
	}()
	var result []string
	for rows.Next() {
		var (
			sequence int
			columnID int
			name     sql.NullString
			desc     int
			collate  sql.NullString
			key      int
		)
		if err := rows.Scan(&sequence, &columnID, &name, &desc, &collate, &key); err != nil {
			return nil, fmt.Errorf("scan index %s: %w", index, err)
		}
		result = append(result, fmt.Sprintf(
			"%d:%d:%s:%d:%s:%d",
			sequence,
			columnID,
			name.String,
			desc,
			collate.String,
			key,
		))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate index %s: %w", index, err)
	}
	return result, nil
}

func readAuxiliarySchemaFingerprint(
	ctx context.Context,
	db database.IQueryer,
) ([]string, error) {
	rows, err := db.QueryContext(
		ctx,
		`SELECT type, name, COALESCE(sql, '')
		 FROM sqlite_master
		 WHERE type IN ('view', 'trigger')
		 ORDER BY type, name`,
	)
	if err != nil {
		return nil, fmt.Errorf("inspect views and triggers: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()
	var result []string
	for rows.Next() {
		var kind, name, statement string
		if err := rows.Scan(&kind, &name, &statement); err != nil {
			return nil, fmt.Errorf("scan view or trigger: %w", err)
		}
		result = append(result, fmt.Sprintf(
			"object|%s|%s|%s",
			kind,
			name,
			strings.ToLower(strings.Join(strings.Fields(statement), " ")),
		))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate views and triggers: %w", err)
	}
	return result, nil
}

func normalizeDefaultValue(value sql.NullString) string {
	if !value.Valid {
		return "<null>"
	}
	return strings.ToLower(strings.Join(strings.Fields(value.String), " "))
}

func quoteIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}
