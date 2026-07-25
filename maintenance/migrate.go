package maintenance

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

const (
	DirectionForward = "forward"
	DirectionReverse = "reverse"
)

var ErrInvalidDirection = errors.New("invalid migration direction")

type PreconditionError struct {
	Message string
}

func (e *PreconditionError) Error() string {
	return e.Message
}

func IsPreconditionError(err error) bool {
	var target *PreconditionError
	return errors.As(err, &target)
}

type PrefixMigrationResult struct {
	Direction       string `json:"direction"`
	DryRun          bool   `json:"dry_run"`
	RootCount       int64  `json:"root_count"`
	SourceCount     int64  `json:"source_count"`
	TargetCount     int64  `json:"target_count"`
	SourceKind      int64  `json:"source_kind"`
	SourceIsDir     bool   `json:"source_is_directory"`
	EntryID         uint64 `json:"entry_id"`
	ParentEntryID   uint64 `json:"parent_entry_id"`
	ChildCount      int64  `json:"child_count"`
	ChangedRows     int64  `json:"changed_rows"`
	AlreadyMigrated bool   `json:"already_migrated"`
}

type prefixEntry struct {
	count         int64
	entryID       uint64
	parentEntryID uint64
	kind          int64
	ctime         int64
	mtime         int64
	refData       string
	fileSize      int64
	fileMode      int64
	childCount    int64
}

type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func prefixNames(direction string) (string, string, error) {
	switch direction {
	case DirectionForward:
		return "defauls", "defaults", nil
	case DirectionReverse:
		return "defaults", "defauls", nil
	default:
		return "", "", fmt.Errorf("%w: %q", ErrInvalidDirection, direction)
	}
}

func readPrefixEntry(
	ctx context.Context,
	database queryer,
	rootEntryID uint64,
	name string,
) (prefixEntry, error) {
	var entry prefixEntry
	if err := database.QueryRowContext(ctx, `
SELECT
    COUNT(*),
    COALESCE(MIN(entry_id), 0),
    COALESCE(MIN(parent_entry_id), 0),
    COALESCE(MIN(file_kind), 0),
    COALESCE(MIN(ctime), 0),
    COALESCE(MIN(mtime), 0),
    COALESCE(MIN(ref_data), ''),
    COALESCE(MIN(file_size), 0),
    COALESCE(MIN(file_mode), 0)
FROM tg_file_mapping_tab
WHERE parent_entry_id = ? AND file_name = ?;`, rootEntryID, name).Scan(
		&entry.count,
		&entry.entryID,
		&entry.parentEntryID,
		&entry.kind,
		&entry.ctime,
		&entry.mtime,
		&entry.refData,
		&entry.fileSize,
		&entry.fileMode,
	); err != nil {
		return prefixEntry{}, fmt.Errorf("inspect /%s entry: %w", name, err)
	}
	if entry.count != 1 {
		return entry, nil
	}
	if err := database.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM tg_file_mapping_tab
WHERE parent_entry_id = ?;`, entry.entryID).Scan(&entry.childCount); err != nil {
		return prefixEntry{}, fmt.Errorf("count /%s children: %w", name, err)
	}
	return entry, nil
}

func inspectPrefixState(
	ctx context.Context,
	database queryer,
	direction string,
	dryRun bool,
) (*PrefixMigrationResult, prefixEntry, prefixEntry, error) {
	sourceName, targetName, err := prefixNames(direction)
	if err != nil {
		return nil, prefixEntry{}, prefixEntry{}, err
	}

	var rootCount int64
	var rootEntryID uint64
	if err := database.QueryRowContext(ctx, `
SELECT COUNT(*), COALESCE(MIN(entry_id), 0)
FROM tg_file_mapping_tab
WHERE parent_entry_id = 0 AND file_name = '/';`).Scan(&rootCount, &rootEntryID); err != nil {
		return nil, prefixEntry{}, prefixEntry{}, fmt.Errorf("inspect root entry: %w", err)
	}
	if rootCount != 1 {
		return nil, prefixEntry{}, prefixEntry{}, &PreconditionError{
			Message: fmt.Sprintf("expected exactly one root entry, got %d", rootCount),
		}
	}

	source, err := readPrefixEntry(ctx, database, rootEntryID, sourceName)
	if err != nil {
		return nil, prefixEntry{}, prefixEntry{}, err
	}
	target, err := readPrefixEntry(ctx, database, rootEntryID, targetName)
	if err != nil {
		return nil, prefixEntry{}, prefixEntry{}, err
	}

	result := &PrefixMigrationResult{
		Direction:       direction,
		DryRun:          dryRun,
		RootCount:       rootCount,
		SourceCount:     source.count,
		TargetCount:     target.count,
		SourceKind:      source.kind,
		SourceIsDir:     source.kind == 1,
		EntryID:         source.entryID,
		ParentEntryID:   source.parentEntryID,
		ChildCount:      source.childCount,
		AlreadyMigrated: source.count == 0 && target.count == 1,
	}
	if result.AlreadyMigrated {
		result.SourceKind = target.kind
		result.SourceIsDir = target.kind == 1
		result.EntryID = target.entryID
		result.ParentEntryID = target.parentEntryID
		result.ChildCount = target.childCount
	}
	return result, source, target, nil
}

func validatePrefixState(result *PrefixMigrationResult, allowAlreadyMigrated bool) error {
	if result.AlreadyMigrated && allowAlreadyMigrated {
		if !result.SourceIsDir {
			return &PreconditionError{
				Message: fmt.Sprintf(
					"already-migrated target is not a directory: kind=%d",
					result.SourceKind,
				),
			}
		}
		return nil
	}
	if result.SourceCount != 1 || result.TargetCount != 0 || !result.SourceIsDir {
		return &PreconditionError{Message: fmt.Sprintf(
			"prefix migration precondition failed: source_count=%d target_count=%d source_kind=%d",
			result.SourceCount,
			result.TargetCount,
			result.SourceKind,
		)}
	}
	return nil
}

func inspectDefaultPrefixMigration(
	ctx context.Context,
	databaseFile string,
	direction string,
) (*PrefixMigrationResult, error) {
	database, err := openDatabase(ctx, databaseFile, true)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = database.Close()
	}()
	result, _, _, err := inspectPrefixState(ctx, database, direction, true)
	if err != nil {
		return nil, err
	}
	if err := validatePrefixState(result, true); err != nil {
		return result, err
	}
	return result, nil
}

func migrateDefaultPrefix(
	ctx context.Context,
	databaseFile string,
	direction string,
) (*PrefixMigrationResult, error) {
	database, err := openDatabase(ctx, databaseFile, false)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = database.Close()
	}()

	connection, err := database.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire migration connection: %w", err)
	}
	defer func() {
		_ = connection.Close()
	}()

	if _, err := connection.ExecContext(ctx, "BEGIN IMMEDIATE;"); err != nil {
		return nil, fmt.Errorf("begin prefix migration: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			rollbackContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), defaultDatabaseTimeout)
			defer cancel()
			_, _ = connection.ExecContext(rollbackContext, "ROLLBACK;")
		}
	}()

	result, source, _, err := inspectPrefixState(ctx, connection, direction, false)
	if err != nil {
		return nil, err
	}
	if err := validatePrefixState(result, false); err != nil {
		return result, err
	}
	sourceName, targetName, _ := prefixNames(direction)
	update, err := connection.ExecContext(ctx, `
UPDATE tg_file_mapping_tab
SET file_name = ?
WHERE entry_id = ?
  AND parent_entry_id = ?
  AND file_name = ?
  AND file_kind = 1;`, targetName, source.entryID, source.parentEntryID, sourceName)
	if err != nil {
		return result, fmt.Errorf("rename /%s to /%s: %w", sourceName, targetName, err)
	}
	changedRows, err := update.RowsAffected()
	if err != nil {
		return result, fmt.Errorf("read changed row count: %w", err)
	}
	result.ChangedRows = changedRows
	if changedRows != 1 {
		return result, &PreconditionError{
			Message: fmt.Sprintf("expected one changed row, got %d", changedRows),
		}
	}

	after, _, target, err := inspectPrefixState(ctx, connection, direction, false)
	if err != nil {
		return result, err
	}
	if !prefixPostconditionMatches(after, source, target) {
		return result, &PreconditionError{Message: "prefix migration postcondition failed; transaction rolled back"}
	}

	if _, err := connection.ExecContext(ctx, "COMMIT;"); err != nil {
		return result, fmt.Errorf("commit prefix migration: %w", err)
	}
	committed = true
	return result, nil
}

func prefixPostconditionMatches(result *PrefixMigrationResult, source, target prefixEntry) bool {
	return result.SourceCount == 0 &&
		result.TargetCount == 1 &&
		target.entryID == source.entryID &&
		target.parentEntryID == source.parentEntryID &&
		target.kind == source.kind &&
		target.ctime == source.ctime &&
		target.mtime == source.mtime &&
		target.refData == source.refData &&
		target.fileSize == source.fileSize &&
		target.fileMode == source.fileMode &&
		target.childCount == source.childCount
}

func MigrateDefaultPrefix(
	ctx context.Context,
	databaseFile string,
	direction string,
	dryRun bool,
) (*PrefixMigrationResult, error) {
	if _, _, err := prefixNames(direction); err != nil {
		return nil, err
	}
	if dryRun {
		return inspectDefaultPrefixMigration(ctx, databaseFile, direction)
	}
	return migrateDefaultPrefix(ctx, databaseFile, direction)
}
