package filemgr

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/xxxsen/common/database"
	"github.com/xxxsen/mimetype"

	"github.com/xxxsen/tgfile/directory"
	"github.com/xxxsen/tgfile/entity"
)

const (
	webDAVLockDepthZero     = "0"
	webDAVLockDepthInfinity = "infinity"
	maxWebDAVPropertySize   = 1 << 20
)

var errWebDAVLockNullNotPrepared = errors.New("WebDAV lock-null file was not prepared")

func WebDAVETag(link *entity.FileLinkMeta) string {
	if link == nil || link.IsDir {
		return ""
	}
	return fmt.Sprintf(`"%d-%d"`, link.FileId, link.FileSize)
}

func (d *defaultFileManager) CreateWebDAVCollection(
	ctx context.Context,
	resourcePath string,
	options WebDAVMutationOptions,
) (*WebDAVMutationResult, error) {
	if err := d.cleanupExpiredWebDAVLocks(ctx); err != nil {
		return nil, err
	}
	var result WebDAVMutationResult
	err := d.objectDir.WithTransaction(ctx, func(ctx context.Context, tx directory.ITransaction) error {
		if err := assertWebDAVConditionsTx(
			ctx,
			tx,
			[]string{resourcePath},
			options.Principal,
			options.Condition,
		); err != nil {
			return err
		}
		if _, exists, err := tx.Stat(ctx, resourcePath); err != nil {
			return fmt.Errorf("stat WebDAV collection target: %w", err)
		} else if exists {
			return os.ErrExist
		}
		if _, err := tx.Mkdir(ctx, resourcePath); err != nil {
			return fmt.Errorf("create WebDAV collection mapping: %w", err)
		}
		result.Created = true
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("create WebDAV collection: %w", err)
	}
	return &result, nil
}

func (d *defaultFileManager) PublishWebDAVFile(
	ctx context.Context,
	resourcePath string,
	fileID uint64,
	size int64,
	options WebDAVMutationOptions,
) (*WebDAVPublishResult, error) {
	if err := d.cleanupExpiredWebDAVLocks(ctx); err != nil {
		return nil, err
	}
	var result WebDAVPublishResult
	err := d.objectDir.WithTransaction(ctx, func(ctx context.Context, tx directory.ITransaction) error {
		return d.publishWebDAVFileTx(
			ctx,
			tx,
			resourcePath,
			fileID,
			size,
			options,
			&result,
		)
	})
	if err != nil {
		return nil, fmt.Errorf("publish WebDAV file: %w", err)
	}
	return &result, nil
}

func (d *defaultFileManager) publishWebDAVFileTx(
	ctx context.Context,
	tx directory.ITransaction,
	resourcePath string,
	fileID uint64,
	size int64,
	options WebDAVMutationOptions,
	result *WebDAVPublishResult,
) error {
	exists, err := prepareWebDAVPublishTx(
		ctx,
		tx,
		resourcePath,
		fileID,
		size,
		options,
	)
	if err != nil {
		return err
	}
	entry, created, err := d.storeWebDAVMappingTx(
		ctx,
		tx,
		resourcePath,
		fileID,
		size,
		exists,
	)
	if err != nil {
		return err
	}
	link, err := webDAVEntryToLink(resourcePath, entry)
	if err != nil {
		return err
	}
	if err := insertS3Metadata(ctx, tx.QueryExecer(), webDAVS3Metadata(link)); err != nil {
		return err
	}
	if _, err := tx.QueryExecer().ExecContext(
		ctx,
		"UPDATE tg_webdav_lock_tab SET lock_null = 0, root_entry_id = ? WHERE root_path = ?",
		entry.EntryID(),
		path.Clean(resourcePath),
	); err != nil {
		return fmt.Errorf("publish WebDAV lock-null resource: %w", err)
	}
	result.Created = created
	result.Link = link
	return nil
}

func prepareWebDAVPublishTx(
	ctx context.Context,
	tx directory.ITransaction,
	resourcePath string,
	fileID uint64,
	size int64,
	options WebDAVMutationOptions,
) (bool, error) {
	currentEntry, exists, err := tx.Stat(ctx, resourcePath)
	if err != nil {
		return false, fmt.Errorf("stat WebDAV publish target: %w", err)
	}
	var currentLink *entity.FileLinkMeta
	if exists {
		currentLink, err = webDAVEntryToLink(resourcePath, currentEntry)
		if err != nil {
			return false, err
		}
		if currentEntry.IsDir() {
			return false, directory.ErrEntryNotFile
		}
	}
	if err := evaluateWebDAVCondition(currentLink, exists, options.Condition); err != nil {
		return false, err
	}
	if err := assertWebDAVLocksTx(
		ctx,
		tx.QueryExecer(),
		[]string{resourcePath},
		options.Principal,
		options.Condition,
	); err != nil {
		return false, err
	}
	if err := enforceWebDAVQuotaTx(
		ctx,
		tx.QueryExecer(),
		options.QuotaRoot,
		options.QuotaBytes,
		currentLink,
		fileID,
		size,
	); err != nil {
		return false, err
	}
	if err := ensureFileTreeCanBeLinked(ctx, tx.QueryExecer(), fileID); err != nil {
		return false, err
	}
	return exists, nil
}

func (d *defaultFileManager) storeWebDAVMappingTx(
	ctx context.Context,
	tx directory.ITransaction,
	resourcePath string,
	fileID uint64,
	size int64,
	exists bool,
) (directory.IDirectoryEntry, bool, error) {
	if !exists {
		if err := ensureWebDAVParentTx(ctx, tx, resourcePath); err != nil {
			return nil, false, err
		}
		entry, err := tx.Create(ctx, resourcePath, size, strconv.FormatUint(fileID, 10))
		if err != nil {
			return nil, false, fmt.Errorf("create WebDAV mapping: %w", err)
		}
		return entry, true, nil
	}
	entry, err := d.replaceWebDAVMappingTx(ctx, tx, resourcePath, fileID, size)
	return entry, false, err
}

func (d *defaultFileManager) replaceWebDAVMappingTx(
	ctx context.Context,
	tx directory.ITransaction,
	resourcePath string,
	fileID uint64,
	size int64,
) (directory.IDirectoryEntry, error) {
	previous, err := tx.Replace(
		ctx,
		resourcePath,
		size,
		strconv.FormatUint(fileID, 10),
		time.Now().UnixMilli(),
	)
	if err != nil {
		return nil, fmt.Errorf("replace WebDAV mapping: %w", err)
	}
	entry, exists, err := tx.Stat(ctx, resourcePath)
	if err != nil {
		return nil, fmt.Errorf("stat replaced WebDAV mapping: %w", err)
	}
	if !exists {
		return nil, os.ErrNotExist
	}
	if err := deleteS3Metadata(ctx, tx.QueryExecer(), entry.EntryID()); err != nil {
		return nil, err
	}
	oldFileIDs, err := mappingEntryFileIDs([]directory.IDirectoryEntry{previous})
	if err != nil {
		return nil, err
	}
	if len(oldFileIDs) != 0 && oldFileIDs[0] != fileID {
		if err := markMappingFilesPending(ctx, tx.QueryExecer(), oldFileIDs); err != nil {
			return nil, err
		}
	}
	return entry, nil
}

func (d *defaultFileManager) DeleteWebDAVResource(
	ctx context.Context,
	resourcePath string,
	options WebDAVMutationOptions,
) error {
	if err := d.cleanupExpiredWebDAVLocks(ctx); err != nil {
		return err
	}
	err := d.objectDir.WithTransaction(ctx, func(ctx context.Context, tx directory.ITransaction) error {
		entry, exists, err := tx.Stat(ctx, resourcePath)
		if err != nil {
			return fmt.Errorf("stat WebDAV delete target: %w", err)
		}
		if !exists {
			return os.ErrNotExist
		}
		link, err := webDAVEntryToLink(resourcePath, entry)
		if err != nil {
			return err
		}
		if err := evaluateWebDAVCondition(link, true, options.Condition); err != nil {
			return err
		}
		if err := enforceWebDAVMutationLimitTx(
			ctx,
			tx.QueryExecer(),
			entry.EntryID(),
			options.MaxEntries,
		); err != nil {
			return err
		}
		if err := assertWebDAVTreeLocksTx(
			ctx,
			tx.QueryExecer(),
			[]string{resourcePath},
			options.Principal,
			options.Condition,
		); err != nil {
			return err
		}
		removed, err := tx.Remove(ctx, resourcePath)
		if err != nil {
			return fmt.Errorf("remove WebDAV resource mapping: %w", err)
		}
		return finalizeRemovedWebDAVEntries(ctx, tx.QueryExecer(), removed)
	})
	if err != nil {
		return fmt.Errorf("delete WebDAV resource: %w", err)
	}
	return nil
}

func (d *defaultFileManager) CopyWebDAVResource(
	ctx context.Context,
	source, destination string,
	overwrite, recursive bool,
	options WebDAVMutationOptions,
) (*WebDAVMutationResult, error) {
	if err := d.cleanupExpiredWebDAVLocks(ctx); err != nil {
		return nil, err
	}
	var result WebDAVMutationResult
	err := d.objectDir.WithTransaction(ctx, func(ctx context.Context, tx directory.ITransaction) error {
		return copyWebDAVResourceTx(
			ctx,
			tx,
			source,
			destination,
			overwrite,
			recursive,
			options,
			&result,
		)
	})
	if err != nil {
		return nil, fmt.Errorf("copy WebDAV resource: %w", err)
	}
	return &result, nil
}

func copyWebDAVResourceTx(
	ctx context.Context,
	tx directory.ITransaction,
	source, destination string,
	overwrite, recursive bool,
	options WebDAVMutationOptions,
	result *WebDAVMutationResult,
) error {
	sourceEntry, sourceExists, err := tx.Stat(ctx, source)
	if err != nil {
		return fmt.Errorf("stat WebDAV copy source: %w", err)
	}
	if !sourceExists {
		return os.ErrNotExist
	}
	sourceLink, err := webDAVEntryToLink(source, sourceEntry)
	if err != nil {
		return err
	}
	if err := evaluateWebDAVCondition(sourceLink, true, options.Condition); err != nil {
		return err
	}
	if recursive {
		if err := enforceWebDAVMutationLimitTx(
			ctx,
			tx.QueryExecer(),
			sourceEntry.EntryID(),
			options.MaxEntries,
		); err != nil {
			return err
		}
	}
	destinationExists, err := prepareWebDAVCopyDestinationTx(
		ctx,
		tx,
		destination,
		options,
	)
	if err != nil {
		return err
	}
	result.Created = !destinationExists
	copies, overwritten, err := tx.CopyDepth(ctx, source, destination, overwrite, recursive)
	if err != nil {
		return fmt.Errorf("copy WebDAV directory entries: %w", err)
	}
	if err := validateWebDAVCopies(ctx, tx.QueryExecer(), copies); err != nil {
		return err
	}
	if err := finalizeRemovedWebDAVEntries(ctx, tx.QueryExecer(), overwritten); err != nil {
		return err
	}
	if err := copyWebDAVMetadata(ctx, tx.QueryExecer(), copies); err != nil {
		return err
	}
	return copyWebDAVProperties(ctx, tx.QueryExecer(), copies)
}

func prepareWebDAVCopyDestinationTx(
	ctx context.Context,
	tx directory.ITransaction,
	destination string,
	options WebDAVMutationOptions,
) (bool, error) {
	if err := ensureWebDAVParentTx(ctx, tx, destination); err != nil {
		return false, err
	}
	_, destinationExists, err := tx.Stat(ctx, destination)
	if err != nil {
		return false, fmt.Errorf("stat WebDAV copy destination: %w", err)
	}
	lockAssert := assertWebDAVLocksTx
	if destinationExists {
		lockAssert = assertWebDAVTreeLocksTx
	}
	if err := lockAssert(
		ctx,
		tx.QueryExecer(),
		[]string{destination},
		options.Principal,
		options.Condition,
	); err != nil {
		return false, err
	}
	return destinationExists, nil
}

func (d *defaultFileManager) MoveWebDAVResource(
	ctx context.Context,
	source, destination string,
	overwrite bool,
	options WebDAVMutationOptions,
) (*WebDAVMutationResult, error) {
	if err := d.cleanupExpiredWebDAVLocks(ctx); err != nil {
		return nil, err
	}
	var result WebDAVMutationResult
	err := d.objectDir.WithTransaction(ctx, func(ctx context.Context, tx directory.ITransaction) error {
		sourceEntry, sourceExists, err := tx.Stat(ctx, source)
		if err != nil {
			return fmt.Errorf("stat WebDAV move source: %w", err)
		}
		if !sourceExists {
			return os.ErrNotExist
		}
		sourceLink, err := webDAVEntryToLink(source, sourceEntry)
		if err != nil {
			return err
		}
		if err := evaluateWebDAVCondition(sourceLink, true, options.Condition); err != nil {
			return err
		}
		if err := enforceWebDAVMutationLimitTx(
			ctx,
			tx.QueryExecer(),
			sourceEntry.EntryID(),
			options.MaxEntries,
		); err != nil {
			return err
		}
		if err := assertWebDAVTreeLocksTx(
			ctx,
			tx.QueryExecer(),
			[]string{source, destination},
			options.Principal,
			options.Condition,
		); err != nil {
			return err
		}
		if err := ensureWebDAVParentTx(ctx, tx, destination); err != nil {
			return err
		}
		_, destinationExists, err := tx.Stat(ctx, destination)
		if err != nil {
			return fmt.Errorf("stat WebDAV move destination: %w", err)
		}
		result.Created = !destinationExists
		overwritten, err := tx.Move(ctx, source, destination, overwrite)
		if err != nil {
			return fmt.Errorf("move WebDAV directory entries: %w", err)
		}
		if err := finalizeRemovedWebDAVEntries(ctx, tx.QueryExecer(), overwritten); err != nil {
			return err
		}
		cleanSource := path.Clean(source)
		cleanDestination := path.Clean(destination)
		if _, err := tx.QueryExecer().ExecContext(
			ctx,
			`UPDATE tg_webdav_lock_tab
SET root_path = ? || substr(root_path, ?)
WHERE root_path = ? OR root_path LIKE ? ESCAPE '\'`,
			cleanDestination,
			len(cleanSource)+1,
			cleanSource,
			escapeSQLiteLike(cleanSource+"/")+"%",
		); err != nil {
			return fmt.Errorf("move WebDAV lock root: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("move WebDAV resource: %w", err)
	}
	return &result, nil
}

func (d *defaultFileManager) ReadWebDAVProperties(
	ctx context.Context,
	entryID uint64,
) ([]WebDAVProperty, error) {
	rows, err := d.dbc.QueryContext(
		ctx,
		`SELECT namespace_uri, local_name, value_xml
FROM tg_webdav_property_tab WHERE entry_id = ? ORDER BY namespace_uri, local_name`,
		entryID,
	)
	if err != nil {
		return nil, fmt.Errorf("query WebDAV properties: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()
	properties := make([]WebDAVProperty, 0)
	for rows.Next() {
		var property WebDAVProperty
		if err := rows.Scan(
			&property.Name.Namespace,
			&property.Name.LocalName,
			&property.ValueXML,
		); err != nil {
			return nil, fmt.Errorf("scan WebDAV property: %w", err)
		}
		properties = append(properties, property)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate WebDAV properties: %w", err)
	}
	return properties, nil
}

func (d *defaultFileManager) PatchWebDAVProperties(
	ctx context.Context,
	resourcePath string,
	patches []WebDAVPropertyPatch,
	options WebDAVMutationOptions,
) error {
	if err := d.cleanupExpiredWebDAVLocks(ctx); err != nil {
		return err
	}
	err := d.objectDir.WithTransaction(ctx, func(ctx context.Context, tx directory.ITransaction) error {
		entry, exists, err := tx.Stat(ctx, resourcePath)
		if err != nil {
			return fmt.Errorf("stat WebDAV property target: %w", err)
		}
		if !exists {
			return os.ErrNotExist
		}
		if err := assertWebDAVConditionsTx(
			ctx,
			tx,
			[]string{resourcePath},
			options.Principal,
			options.Condition,
		); err != nil {
			return err
		}
		now := time.Now().UnixMilli()
		for _, patch := range patches {
			if err := validateWebDAVDeadProperty(patch.Property); err != nil {
				return err
			}
			if patch.Set {
				if _, err := tx.QueryExecer().ExecContext(
					ctx,
					`INSERT INTO tg_webdav_property_tab (
entry_id, namespace_uri, local_name, value_xml, ctime, mtime
) VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(entry_id, namespace_uri, local_name)
DO UPDATE SET value_xml = excluded.value_xml, mtime = excluded.mtime`,
					entry.EntryID(),
					patch.Property.Name.Namespace,
					patch.Property.Name.LocalName,
					patch.Property.ValueXML,
					now,
					now,
				); err != nil {
					return fmt.Errorf("set WebDAV property: %w", err)
				}
				continue
			}
			if _, err := tx.QueryExecer().ExecContext(
				ctx,
				`DELETE FROM tg_webdav_property_tab
WHERE entry_id = ? AND namespace_uri = ? AND local_name = ?`,
				entry.EntryID(),
				patch.Property.Name.Namespace,
				patch.Property.Name.LocalName,
			); err != nil {
				return fmt.Errorf("remove WebDAV property: %w", err)
			}
		}
		return tx.Touch(ctx, resourcePath, now)
	})
	if err != nil {
		return fmt.Errorf("patch WebDAV properties: %w", err)
	}
	return nil
}

func (d *defaultFileManager) ListWebDAVLocks(
	ctx context.Context,
	resourcePath string,
) ([]WebDAVLock, error) {
	if err := d.cleanupExpiredWebDAVLocks(ctx); err != nil {
		return nil, err
	}
	return queryApplicableWebDAVLocks(ctx, d.dbc, resourcePath)
}

func (d *defaultFileManager) LockWebDAVResource(
	ctx context.Context,
	request WebDAVLockRequest,
) (*WebDAVLockResult, error) {
	if err := d.cleanupExpiredWebDAVLocks(ctx); err != nil {
		return nil, err
	}
	unpublishedFileID, err := d.prepareWebDAVLockNullFile(ctx, request.Path)
	if err != nil {
		return nil, err
	}
	cleanup := func() {
		if unpublishedFileID == 0 {
			return
		}
		cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), deleteTimeout)
		defer cancel()
		_ = d.DiscardUnpublishedFile(cleanupContext, unpublishedFileID)
	}

	var result WebDAVLockResult
	err = d.objectDir.WithTransaction(ctx, func(ctx context.Context, tx directory.ITransaction) error {
		return d.lockWebDAVResourceTx(ctx, tx, request, unpublishedFileID, &result)
	})
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("lock WebDAV resource: %w", err)
	}
	if !result.Created {
		cleanup()
	} else {
		unpublishedFileID = 0
	}
	return &result, nil
}

func (d *defaultFileManager) prepareWebDAVLockNullFile(
	ctx context.Context,
	resourcePath string,
) (uint64, error) {
	if _, err := d.objectDir.Stat(ctx, resourcePath); err == nil {
		return 0, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return 0, fmt.Errorf("stat WebDAV lock target: %w", err)
	}
	fileID, err := d.CreateFile(ctx, 0, strings.NewReader(""))
	if err != nil {
		return 0, fmt.Errorf("create WebDAV lock-null file: %w", err)
	}
	return fileID, nil
}

func (d *defaultFileManager) lockWebDAVResourceTx(
	ctx context.Context,
	tx directory.ITransaction,
	request WebDAVLockRequest,
	unpublishedFileID uint64,
	result *WebDAVLockResult,
) error {
	entry, created, err := ensureWebDAVLockTargetTx(
		ctx,
		tx,
		request,
		unpublishedFileID,
	)
	if err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	lock := WebDAVLock{
		Token:       "opaquelocktoken:" + uuid.NewString(),
		RootPath:    path.Clean(request.Path),
		RootEntryID: entry.EntryID(),
		Depth:       request.Depth,
		OwnerXML:    request.OwnerXML,
		Principal:   request.Principal,
		CreatedAt:   now,
		ExpiresAt:   now + request.Timeout.Milliseconds(),
		LockNull:    created,
	}
	if _, err := tx.QueryExecer().ExecContext(
		ctx,
		`INSERT INTO tg_webdav_lock_tab (
token, root_path, root_entry_id, lock_depth, owner_xml, principal,
created_at, expires_at, lock_null
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		lock.Token,
		lock.RootPath,
		lock.RootEntryID,
		lock.Depth,
		lock.OwnerXML,
		lock.Principal,
		lock.CreatedAt,
		lock.ExpiresAt,
		boolToInteger(lock.LockNull),
	); err != nil {
		return fmt.Errorf("create WebDAV lock: %w", err)
	}
	result.Created = created
	result.Lock = lock
	return nil
}

func ensureWebDAVLockTargetTx(
	ctx context.Context,
	tx directory.ITransaction,
	request WebDAVLockRequest,
	unpublishedFileID uint64,
) (directory.IDirectoryEntry, bool, error) {
	entry, exists, err := tx.Stat(ctx, request.Path)
	if err != nil {
		return nil, false, fmt.Errorf("stat WebDAV lock target: %w", err)
	}
	if exists {
		if err := assertNoConflictingWebDAVLockTx(
			ctx,
			tx.QueryExecer(),
			request.Path,
			request.Depth,
		); err != nil {
			return nil, false, err
		}
		return entry, false, nil
	}
	if err := ensureWebDAVParentTx(ctx, tx, request.Path); err != nil {
		return nil, false, err
	}
	if unpublishedFileID == 0 {
		return nil, false, errWebDAVLockNullNotPrepared
	}
	entry, err = tx.Create(
		ctx,
		request.Path,
		0,
		strconv.FormatUint(unpublishedFileID, 10),
	)
	if err != nil {
		return nil, false, fmt.Errorf("create WebDAV lock-null mapping: %w", err)
	}
	return entry, true, nil
}

func (d *defaultFileManager) RefreshWebDAVLock(
	ctx context.Context,
	resourcePath, token, principal string,
	timeout time.Duration,
	ifHeader *WebDAVIfHeader,
) (*WebDAVLock, error) {
	if err := d.cleanupExpiredWebDAVLocks(ctx); err != nil {
		return nil, err
	}
	var refreshed WebDAVLock
	err := d.dbc.OnTransation(ctx, func(ctx context.Context, tx database.IQueryExecer) error {
		lock, found, err := queryWebDAVLockByToken(ctx, tx, token)
		if err != nil {
			return err
		}
		if !found || lock.RootPath != path.Clean(resourcePath) || lock.Principal != principal {
			return ErrWebDAVLockToken
		}
		condition := &WebDAVCondition{
			IfHeader:    ifHeader,
			RequestPath: path.Clean(resourcePath),
		}
		if !webDAVIfHeaderAllows(condition, resourcePath, "", []WebDAVLock{lock}) {
			return ErrWebDAVPrecondition
		}
		lock.ExpiresAt = time.Now().UnixMilli() + timeout.Milliseconds()
		if _, err := tx.ExecContext(
			ctx,
			"UPDATE tg_webdav_lock_tab SET expires_at = ? WHERE token = ?",
			lock.ExpiresAt,
			lock.Token,
		); err != nil {
			return fmt.Errorf("refresh WebDAV lock: %w", err)
		}
		refreshed = lock
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("refresh WebDAV lock: %w", err)
	}
	return &refreshed, nil
}

func (d *defaultFileManager) UnlockWebDAVResource(
	ctx context.Context,
	resourcePath, token, principal string,
) error {
	if err := d.cleanupExpiredWebDAVLocks(ctx); err != nil {
		return err
	}
	err := d.objectDir.WithTransaction(ctx, func(ctx context.Context, tx directory.ITransaction) error {
		lock, found, err := queryWebDAVLockByToken(ctx, tx.QueryExecer(), token)
		if err != nil {
			return err
		}
		if !found || lock.RootPath != path.Clean(resourcePath) || lock.Principal != principal {
			return ErrWebDAVLockToken
		}
		if _, err := tx.QueryExecer().ExecContext(
			ctx,
			"DELETE FROM tg_webdav_lock_tab WHERE token = ?",
			token,
		); err != nil {
			return fmt.Errorf("delete WebDAV lock: %w", err)
		}
		if !lock.LockNull {
			return nil
		}
		removed, err := tx.Remove(ctx, resourcePath)
		if err != nil {
			return fmt.Errorf("remove WebDAV lock-null mapping: %w", err)
		}
		return finalizeRemovedWebDAVEntries(ctx, tx.QueryExecer(), removed)
	})
	if err != nil {
		return fmt.Errorf("unlock WebDAV resource: %w", err)
	}
	return nil
}

func (d *defaultFileManager) WebDAVQuota(
	ctx context.Context,
	root string,
	limit int64,
) (int64, int64, error) {
	used, err := queryWebDAVQuotaUsed(ctx, d.dbc, root)
	if err != nil {
		return 0, 0, err
	}
	available := max(0, limit-used)
	return used, available, nil
}

func (d *defaultFileManager) WebDAVChanges(
	ctx context.Context,
	root string,
	since int64,
	depth string,
	limit int,
) (*WebDAVChangePage, error) {
	if limit <= 0 {
		limit = 1000
	}
	current, err := currentWebDAVRevision(ctx, d.dbc)
	if err != nil {
		return nil, err
	}
	if since < 0 {
		return &WebDAVChangePage{SyncRevision: current}, nil
	}
	if since > current {
		return nil, ErrWebDAVSyncToken
	}
	rows, err := queryWebDAVChanges(ctx, d.dbc, root, since, depth, limit)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = rows.Close()
	}()
	return scanWebDAVChanges(rows, path.Clean(root), depth, limit, current)
}

func currentWebDAVRevision(
	ctx context.Context,
	queryer database.IQueryExecer,
) (int64, error) {
	var current int64
	if err := queryRow(
		ctx,
		queryer,
		"SELECT COALESCE(MAX(revision), 0) FROM tg_webdav_change_tab",
	).Scan(&current); err != nil {
		return 0, fmt.Errorf("read WebDAV sync revision: %w", err)
	}
	return current, nil
}

func queryWebDAVChanges(
	ctx context.Context,
	queryer database.IQueryExecer,
	root string,
	since int64,
	depth string,
	limit int,
) (*sql.Rows, error) {
	cleanRoot := path.Clean(root)
	rows, err := queryer.QueryContext(
		ctx,
		`WITH latest AS (
SELECT path, MAX(revision) AS revision
FROM tg_webdav_change_tab
WHERE revision > ?
  AND (
      (? = '/' AND path LIKE '/%') OR
      path = ? OR
      path LIKE ? ESCAPE '\'
  )
  AND (
      ? = 'infinity' OR
      path = ? OR
      instr(substr(path, ?), '/') = 0
  )
GROUP BY path
)
SELECT change.revision, change.path, change.change_kind
FROM latest
JOIN tg_webdav_change_tab change ON change.revision = latest.revision
ORDER BY change.revision
LIMIT ?`,
		since,
		cleanRoot,
		cleanRoot,
		escapeSQLiteLike(strings.TrimSuffix(cleanRoot, "/")+"/")+"%",
		depth,
		cleanRoot,
		len(cleanRoot)+2,
		limit+1,
	)
	if err != nil {
		return nil, fmt.Errorf("query WebDAV changes: %w", err)
	}
	return rows, nil
}

func scanWebDAVChanges(
	rows *sql.Rows,
	root, depth string,
	limit int,
	current int64,
) (*WebDAVChangePage, error) {
	result := &WebDAVChangePage{SyncRevision: current}
	for rows.Next() {
		var change WebDAVChange
		if err := rows.Scan(&change.Revision, &change.Path, &change.Kind); err != nil {
			return nil, fmt.Errorf("scan WebDAV change: %w", err)
		}
		if !webDAVChangeInScope(root, change.Path, depth) {
			continue
		}
		if len(result.Changes) == limit {
			result.HasMore = true
			result.SyncRevision = result.Changes[len(result.Changes)-1].Revision
			break
		}
		result.Changes = append(result.Changes, change)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate WebDAV changes: %w", err)
	}
	return result, nil
}

func ensureWebDAVParentTx(
	ctx context.Context,
	tx directory.ITransaction,
	resourcePath string,
) error {
	parentPath := path.Dir(path.Clean(resourcePath))
	parent, exists, err := tx.Stat(ctx, parentPath)
	if err != nil {
		return fmt.Errorf("stat WebDAV parent: %w", err)
	}
	if !exists {
		return directory.ErrParentNotFound
	}
	if !parent.IsDir() {
		return directory.ErrPathComponentNotDirectory
	}
	return nil
}

func webDAVEntryToLink(
	resourcePath string,
	entry directory.IDirectoryEntry,
) (*entity.FileLinkMeta, error) {
	if entry == nil {
		return nil, os.ErrNotExist
	}
	fileID := uint64(0)
	if !entry.IsDir() {
		var err error
		fileID, err = strconv.ParseUint(entry.RefData(), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse WebDAV mapping file id: %w", err)
		}
	}
	return &entity.FileLinkMeta{
		EntryID:  entry.EntryID(),
		FileName: path.Base(resourcePath),
		FileId:   fileID,
		FileSize: entry.Size(),
		Mode:     entry.Mode(),
		Ctime:    entry.Ctime(),
		Mtime:    entry.Mtime(),
		IsDir:    entry.IsDir(),
	}, nil
}

func webDAVS3Metadata(link *entity.FileLinkMeta) *entity.S3ObjectMetadata {
	extension := ""
	if index := strings.LastIndexByte(link.FileName, '.'); index >= 0 {
		extension = link.FileName[index:]
	}
	now := time.Now().UnixMilli()
	return &entity.S3ObjectMetadata{
		EntryID:      link.EntryID,
		ETag:         WebDAVETag(link),
		ContentType:  mimetype.LookupWithDefault(extension, "application/octet-stream"),
		CacheControl: defaultS3CacheControl,
		UserMetadata: "{}",
		Ctime:        now,
		Mtime:        now,
	}
}

func evaluateWebDAVCondition(
	link *entity.FileLinkMeta,
	exists bool,
	condition *WebDAVCondition,
) error {
	if condition == nil {
		return nil
	}
	etag := WebDAVETag(link)
	if err := evaluateWebDAVETagCondition(etag, exists, condition); err != nil {
		return err
	}
	return evaluateWebDAVTimeCondition(link, exists, condition)
}

func evaluateWebDAVETagCondition(
	etag string,
	exists bool,
	condition *WebDAVCondition,
) error {
	if condition.IfMatch != "" &&
		(!exists || !webDAVETagListContains(condition.IfMatch, etag, false)) {
		return ErrWebDAVPrecondition
	}
	if condition.IfNoneMatch != "" &&
		exists &&
		(condition.IfNoneMatch == "*" ||
			webDAVETagListContains(condition.IfNoneMatch, etag, true)) {
		return ErrWebDAVPrecondition
	}
	return nil
}

func evaluateWebDAVTimeCondition(
	link *entity.FileLinkMeta,
	exists bool,
	condition *WebDAVCondition,
) error {
	if !exists {
		return nil
	}
	modified := time.UnixMilli(link.Mtime).Truncate(time.Second)
	if condition.IfMatch == "" &&
		condition.IfUnmodifiedSince != nil &&
		modified.After(condition.IfUnmodifiedSince.Truncate(time.Second)) {
		return ErrWebDAVPrecondition
	}
	if condition.IfNoneMatch == "" &&
		condition.IfModifiedSince != nil &&
		!modified.After(condition.IfModifiedSince.Truncate(time.Second)) {
		return ErrWebDAVPrecondition
	}
	return nil
}

func webDAVETagListContains(value, etag string, weak bool) bool {
	if value == "*" {
		return true
	}
	if weak {
		etag = strings.TrimPrefix(etag, "W/")
	}
	for _, candidate := range strings.Split(value, ",") {
		candidate = strings.TrimSpace(candidate)
		if weak {
			candidate = strings.TrimPrefix(candidate, "W/")
		}
		if candidate == etag {
			return true
		}
	}
	return false
}

func assertWebDAVConditionsTx(
	ctx context.Context,
	tx directory.ITransaction,
	paths []string,
	principal string,
	condition *WebDAVCondition,
) error {
	if err := evaluateFirstWebDAVConditionTx(ctx, tx, paths, condition); err != nil {
		return err
	}
	return assertWebDAVLocksTx(ctx, tx.QueryExecer(), paths, principal, condition)
}

func evaluateFirstWebDAVConditionTx(
	ctx context.Context,
	tx directory.ITransaction,
	paths []string,
	condition *WebDAVCondition,
) error {
	if len(paths) == 0 {
		return nil
	}
	entry, exists, err := tx.Stat(ctx, paths[0])
	if err != nil {
		return fmt.Errorf("stat WebDAV condition target: %w", err)
	}
	var link *entity.FileLinkMeta
	if exists {
		link, err = webDAVEntryToLink(paths[0], entry)
		if err != nil {
			return err
		}
	}
	return evaluateWebDAVCondition(link, exists, condition)
}

func assertWebDAVLocksTx(
	ctx context.Context,
	queryExecer database.IQueryExecer,
	paths []string,
	_ string,
	condition *WebDAVCondition,
) error {
	for _, resourcePath := range paths {
		locks, err := queryApplicableWebDAVLocks(ctx, queryExecer, resourcePath)
		if err != nil {
			return err
		}
		if len(locks) == 0 &&
			(condition == nil || condition.IfHeader == nil ||
				!hasApplicableWebDAVIfList(condition, resourcePath)) {
			continue
		}
		etag, err := queryWebDAVETagByPath(ctx, queryExecer, resourcePath)
		if err != nil {
			return err
		}
		if len(locks) == 0 {
			if !webDAVIfHeaderAllows(condition, resourcePath, etag, nil) {
				return ErrWebDAVPrecondition
			}
			continue
		}
		for _, lock := range locks {
			if webDAVIfHeaderAllows(condition, resourcePath, etag, []WebDAVLock{lock}) {
				continue
			}
			return ErrWebDAVLocked
		}
	}
	return nil
}

func assertWebDAVTreeLocksTx(
	ctx context.Context,
	queryExecer database.IQueryExecer,
	paths []string,
	_ string,
	condition *WebDAVCondition,
) error {
	for _, resourcePath := range paths {
		locks, err := queryOverlappingWebDAVLocks(ctx, queryExecer, resourcePath)
		if err != nil {
			return err
		}
		etag, err := queryWebDAVETagByPath(ctx, queryExecer, resourcePath)
		if err != nil {
			return err
		}
		for _, lock := range locks {
			if webDAVIfHeaderAllows(condition, resourcePath, etag, []WebDAVLock{lock}) {
				continue
			}
			return ErrWebDAVLocked
		}
		if len(locks) == 0 &&
			condition != nil &&
			condition.IfHeader != nil &&
			hasApplicableWebDAVIfList(condition, resourcePath) &&
			!webDAVIfHeaderAllows(condition, resourcePath, etag, nil) {
			return ErrWebDAVPrecondition
		}
	}
	return nil
}

func hasApplicableWebDAVIfList(condition *WebDAVCondition, resourcePath string) bool {
	if condition == nil || condition.IfHeader == nil {
		return false
	}
	requestPath := path.Clean(condition.RequestPath)
	resourcePath = path.Clean(resourcePath)
	for _, list := range condition.IfHeader.Lists {
		target := requestPath
		if list.Resource != "" {
			target = path.Clean(list.Resource)
		}
		if target == resourcePath {
			return true
		}
	}
	return false
}

func webDAVIfHeaderAllows(
	condition *WebDAVCondition,
	resourcePath, etag string,
	locks []WebDAVLock,
) bool {
	if condition == nil || condition.IfHeader == nil {
		return false
	}
	requestPath := path.Clean(condition.RequestPath)
	resourcePath = path.Clean(resourcePath)
	for _, list := range condition.IfHeader.Lists {
		if !webDAVIfListApplies(list, requestPath, resourcePath) {
			continue
		}
		if webDAVIfListSatisfied(list, etag, locks) {
			return true
		}
	}
	return false
}

func webDAVIfListApplies(
	list WebDAVIfList,
	requestPath, resourcePath string,
) bool {
	target := requestPath
	if list.Resource != "" {
		target = path.Clean(list.Resource)
	}
	return target == resourcePath
}

func webDAVIfListSatisfied(
	list WebDAVIfList,
	etag string,
	locks []WebDAVLock,
) bool {
	for _, term := range list.Terms {
		if !webDAVIfTermSatisfied(term, etag, locks) {
			return false
		}
	}
	return true
}

func webDAVIfTermSatisfied(
	term WebDAVIfTerm,
	etag string,
	locks []WebDAVLock,
) bool {
	satisfied := false
	switch {
	case term.LockToken != "":
		for _, lock := range locks {
			if lock.Token == term.LockToken {
				satisfied = true
				break
			}
		}
	case term.ETag != "":
		satisfied = webDAVETagListContains(term.ETag, etag, true)
	}
	if term.Not {
		return !satisfied
	}
	return satisfied
}

func queryWebDAVETagByPath(
	ctx context.Context,
	queryer database.IQueryer,
	resourcePath string,
) (string, error) {
	const query = `WITH RECURSIVE tree (
entry_id, ref_data, file_kind, file_size, full_path
) AS (
SELECT entry_id, ref_data, file_kind, file_size, '/'
FROM tg_file_mapping_tab WHERE parent_entry_id = 0 AND file_name = '/'
UNION ALL
SELECT child.entry_id, child.ref_data, child.file_kind, child.file_size,
CASE WHEN tree.full_path = '/' THEN '/' || child.file_name
ELSE tree.full_path || '/' || child.file_name END
FROM tg_file_mapping_tab child
JOIN tree ON child.parent_entry_id = tree.entry_id
)
SELECT ref_data, file_kind, file_size FROM tree WHERE full_path = ?`
	var (
		refData  string
		fileKind int
		fileSize int64
	)
	err := queryRow(ctx, queryer, query, path.Clean(resourcePath)).Scan(
		&refData,
		&fileKind,
		&fileSize,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("query WebDAV condition ETag: %w", err)
	}
	if fileKind != 2 {
		return "", nil
	}
	fileID, err := strconv.ParseUint(refData, 10, 64)
	if err != nil {
		return "", fmt.Errorf("parse WebDAV condition file id: %w", err)
	}
	return fmt.Sprintf(`"%d-%d"`, fileID, fileSize), nil
}

func (d *defaultFileManager) cleanupExpiredWebDAVLocks(ctx context.Context) error {
	err := d.objectDir.WithTransaction(ctx, func(ctx context.Context, tx directory.ITransaction) error {
		expired, err := queryWebDAVLocks(
			ctx,
			tx.QueryExecer(),
			`SELECT token, root_path, root_entry_id, lock_depth, owner_xml, principal,
created_at, expires_at, lock_null
FROM tg_webdav_lock_tab
WHERE expires_at <= ?
ORDER BY expires_at`,
			time.Now().UnixMilli(),
		)
		if err != nil {
			return err
		}
		for _, lock := range expired {
			if _, err := tx.QueryExecer().ExecContext(
				ctx,
				"DELETE FROM tg_webdav_lock_tab WHERE token = ?",
				lock.Token,
			); err != nil {
				return fmt.Errorf("delete expired WebDAV lock: %w", err)
			}
			if !lock.LockNull {
				continue
			}
			entry, exists, err := tx.Stat(ctx, lock.RootPath)
			if err != nil {
				return fmt.Errorf("stat expired WebDAV lock-null resource: %w", err)
			}
			if !exists || entry.EntryID() != lock.RootEntryID {
				continue
			}
			removed, err := tx.Remove(ctx, lock.RootPath)
			if err != nil {
				return fmt.Errorf("remove expired WebDAV lock-null resource: %w", err)
			}
			if err := finalizeRemovedWebDAVEntries(
				ctx,
				tx.QueryExecer(),
				removed,
			); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("clean expired WebDAV locks: %w", err)
	}
	return nil
}

func queryApplicableWebDAVLocks(
	ctx context.Context,
	queryer database.IQueryer,
	resourcePath string,
) ([]WebDAVLock, error) {
	return queryWebDAVLocks(
		ctx,
		queryer,
		`SELECT token, root_path, root_entry_id, lock_depth, owner_xml, principal,
created_at, expires_at, lock_null
FROM tg_webdav_lock_tab
WHERE expires_at > ? AND (
root_path = ? OR
(lock_depth = 'infinity' AND (
(root_path = '/' AND ? LIKE '/%') OR ? LIKE root_path || '/%'
))
)
ORDER BY root_path`,
		time.Now().UnixMilli(),
		path.Clean(resourcePath),
		path.Clean(resourcePath),
		path.Clean(resourcePath),
	)
}

func queryOverlappingWebDAVLocks(
	ctx context.Context,
	queryer database.IQueryer,
	resourcePath string,
) ([]WebDAVLock, error) {
	cleaned := path.Clean(resourcePath)
	return queryWebDAVLocks(
		ctx,
		queryer,
		`SELECT token, root_path, root_entry_id, lock_depth, owner_xml, principal,
created_at, expires_at, lock_null
FROM tg_webdav_lock_tab
WHERE expires_at > ? AND (
root_path = ? OR
(lock_depth = 'infinity' AND (
(root_path = '/' AND ? LIKE '/%') OR ? LIKE root_path || '/%'
)) OR
root_path LIKE ? ESCAPE '\'
)
ORDER BY root_path`,
		time.Now().UnixMilli(),
		cleaned,
		cleaned,
		cleaned,
		escapeSQLiteLike(cleaned+"/")+"%",
	)
}

func queryWebDAVLocks(
	ctx context.Context,
	queryer database.IQueryer,
	query string,
	args ...any,
) ([]WebDAVLock, error) {
	rows, err := queryer.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query WebDAV locks: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()
	locks := make([]WebDAVLock, 0)
	for rows.Next() {
		var lock WebDAVLock
		var lockNull int
		if err := rows.Scan(
			&lock.Token,
			&lock.RootPath,
			&lock.RootEntryID,
			&lock.Depth,
			&lock.OwnerXML,
			&lock.Principal,
			&lock.CreatedAt,
			&lock.ExpiresAt,
			&lockNull,
		); err != nil {
			return nil, fmt.Errorf("scan WebDAV lock: %w", err)
		}
		lock.LockNull = lockNull != 0
		locks = append(locks, lock)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate WebDAV locks: %w", err)
	}
	return locks, nil
}

func queryWebDAVLockByToken(
	ctx context.Context,
	queryer database.IQueryer,
	token string,
) (WebDAVLock, bool, error) {
	var lock WebDAVLock
	var lockNull int
	err := queryRow(
		ctx,
		queryer,
		`SELECT token, root_path, root_entry_id, lock_depth, owner_xml, principal,
created_at, expires_at, lock_null
FROM tg_webdav_lock_tab WHERE token = ? AND expires_at > ?`,
		token,
		time.Now().UnixMilli(),
	).Scan(
		&lock.Token,
		&lock.RootPath,
		&lock.RootEntryID,
		&lock.Depth,
		&lock.OwnerXML,
		&lock.Principal,
		&lock.CreatedAt,
		&lock.ExpiresAt,
		&lockNull,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return WebDAVLock{}, false, nil
	}
	if err != nil {
		return WebDAVLock{}, false, fmt.Errorf("query WebDAV lock token: %w", err)
	}
	lock.LockNull = lockNull != 0
	return lock, true, nil
}

func assertNoConflictingWebDAVLockTx(
	ctx context.Context,
	queryer database.IQueryer,
	resourcePath, depth string,
) error {
	query := queryApplicableWebDAVLocks
	if depth == webDAVLockDepthInfinity {
		query = queryOverlappingWebDAVLocks
	}
	locks, err := query(ctx, queryer, resourcePath)
	if err != nil {
		return err
	}
	if len(locks) != 0 {
		return ErrWebDAVLocked
	}
	return nil
}

func validateWebDAVDeadProperty(property WebDAVProperty) error {
	if property.Name.Namespace == "DAV:" ||
		strings.TrimSpace(property.Name.LocalName) == "" ||
		len(property.ValueXML) > maxWebDAVPropertySize {
		return ErrWebDAVProperty
	}
	return nil
}

func finalizeRemovedWebDAVEntries(
	ctx context.Context,
	queryExecer database.IQueryExecer,
	entries []directory.IDirectoryEntry,
) error {
	fileIDs, err := mappingEntryFileIDs(entries)
	if err != nil {
		return err
	}
	if err := deleteMappingMetadata(ctx, queryExecer, entries); err != nil {
		return err
	}
	if err := deleteWebDAVProperties(ctx, queryExecer, entries); err != nil {
		return err
	}
	if err := deleteWebDAVLocks(ctx, queryExecer, entries); err != nil {
		return err
	}
	return markMappingFilesPending(ctx, queryExecer, fileIDs)
}

func deleteWebDAVProperties(
	ctx context.Context,
	exec database.IExecer,
	entries []directory.IDirectoryEntry,
) error {
	for _, entry := range entries {
		if _, err := exec.ExecContext(
			ctx,
			"DELETE FROM tg_webdav_property_tab WHERE entry_id = ?",
			entry.EntryID(),
		); err != nil {
			return fmt.Errorf("delete WebDAV properties: %w", err)
		}
	}
	return nil
}

func deleteWebDAVLocks(
	ctx context.Context,
	exec database.IExecer,
	entries []directory.IDirectoryEntry,
) error {
	for _, entry := range entries {
		if _, err := exec.ExecContext(
			ctx,
			"DELETE FROM tg_webdav_lock_tab WHERE root_entry_id = ?",
			entry.EntryID(),
		); err != nil {
			return fmt.Errorf("delete WebDAV locks: %w", err)
		}
	}
	return nil
}

func copyWebDAVProperties(
	ctx context.Context,
	exec database.IExecer,
	copies []directory.EntryCopy,
) error {
	const statement = `INSERT INTO tg_webdav_property_tab (
entry_id, namespace_uri, local_name, value_xml, ctime, mtime
)
SELECT ?, namespace_uri, local_name, value_xml, ?, ?
FROM tg_webdav_property_tab WHERE entry_id = ?`
	now := time.Now().UnixMilli()
	for _, copied := range copies {
		if _, err := exec.ExecContext(
			ctx,
			statement,
			copied.Destination.EntryID(),
			now,
			now,
			copied.Source.EntryID(),
		); err != nil {
			return fmt.Errorf("copy WebDAV properties: %w", err)
		}
	}
	return nil
}

func rebindWebDAVProtocolState(
	ctx context.Context,
	exec database.IExecer,
	sourceEntryID, destinationEntryID uint64,
	resourcePath string,
) error {
	if _, err := exec.ExecContext(
		ctx,
		"UPDATE tg_webdav_property_tab SET entry_id = ? WHERE entry_id = ?",
		destinationEntryID,
		sourceEntryID,
	); err != nil {
		return fmt.Errorf("rebind WebDAV properties: %w", err)
	}
	if _, err := exec.ExecContext(
		ctx,
		`UPDATE tg_webdav_lock_tab
SET root_entry_id = ?, root_path = ?
WHERE root_entry_id = ?`,
		destinationEntryID,
		path.Clean(resourcePath),
		sourceEntryID,
	); err != nil {
		return fmt.Errorf("rebind WebDAV locks: %w", err)
	}
	return nil
}

type linkDirectoryEntry struct {
	link *entity.FileLinkMeta
}

func (e linkDirectoryEntry) EntryID() uint64 { return e.link.EntryID }
func (e linkDirectoryEntry) RefData() string {
	return strconv.FormatUint(e.link.FileId, 10)
}
func (e linkDirectoryEntry) Name() string { return e.link.FileName }
func (e linkDirectoryEntry) IsDir() bool  { return e.link.IsDir }
func (e linkDirectoryEntry) Ctime() int64 { return e.link.Ctime }
func (e linkDirectoryEntry) Mtime() int64 { return e.link.Mtime }
func (e linkDirectoryEntry) Mode() uint32 { return e.link.Mode }
func (e linkDirectoryEntry) Size() int64  { return e.link.FileSize }

func enforceWebDAVMutationLimitTx(
	ctx context.Context,
	queryer database.IQueryer,
	rootEntryID uint64,
	limit int,
) error {
	if limit <= 0 {
		return nil
	}
	var count int
	if err := queryRow(
		ctx,
		queryer,
		`WITH RECURSIVE subtree(entry_id) AS (
SELECT entry_id FROM tg_file_mapping_tab WHERE entry_id = ?
UNION ALL
SELECT child.entry_id
FROM tg_file_mapping_tab child
JOIN subtree parent ON child.parent_entry_id = parent.entry_id
)
SELECT COUNT(*) FROM subtree`,
		rootEntryID,
	).Scan(&count); err != nil {
		return fmt.Errorf("count WebDAV mutation entries: %w", err)
	}
	if count > limit {
		return ErrWebDAVTooManyItems
	}
	return nil
}

func queryWebDAVQuotaUsed(
	ctx context.Context,
	queryer database.IQueryer,
	root string,
) (int64, error) {
	entryID, err := queryWebDAVRootEntryID(ctx, queryer, root)
	if err != nil {
		return 0, err
	}
	var used int64
	if err := queryRow(
		ctx,
		queryer,
		`WITH RECURSIVE subtree(entry_id, ref_data, file_kind, file_size) AS (
SELECT entry_id, ref_data, file_kind, file_size
FROM tg_file_mapping_tab WHERE entry_id = ?
UNION ALL
SELECT child.entry_id, child.ref_data, child.file_kind, child.file_size
FROM tg_file_mapping_tab child
JOIN subtree parent ON child.parent_entry_id = parent.entry_id
),
unique_files AS (
SELECT ref_data, MAX(file_size) AS file_size
FROM subtree WHERE file_kind = 2
GROUP BY ref_data
)
SELECT COALESCE(SUM(file_size), 0) FROM unique_files`,
		entryID,
	).Scan(&used); err != nil {
		return 0, fmt.Errorf("calculate WebDAV quota: %w", err)
	}
	return used, nil
}

func queryWebDAVRootEntryID(
	ctx context.Context,
	queryer database.IQueryer,
	root string,
) (uint64, error) {
	root = path.Clean(root)
	if root == "/" {
		var entryID uint64
		err := queryRow(
			ctx,
			queryer,
			`SELECT entry_id FROM tg_file_mapping_tab
WHERE parent_entry_id = 0 AND file_name = '/'`,
		).Scan(&entryID)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, os.ErrNotExist
		}
		if err != nil {
			return 0, fmt.Errorf("query WebDAV root: %w", err)
		}
		return entryID, nil
	}
	const query = `WITH RECURSIVE tree(entry_id, parent_entry_id, file_name, full_path) AS (
SELECT entry_id, parent_entry_id, file_name, '/'
FROM tg_file_mapping_tab WHERE parent_entry_id = 0 AND file_name = '/'
UNION ALL
SELECT child.entry_id, child.parent_entry_id, child.file_name,
CASE WHEN tree.full_path = '/' THEN '/' || child.file_name
ELSE tree.full_path || '/' || child.file_name END
FROM tg_file_mapping_tab child
JOIN tree ON child.parent_entry_id = tree.entry_id
)
SELECT entry_id FROM tree WHERE full_path = ?`
	var entryID uint64
	err := queryRow(ctx, queryer, query, root).Scan(&entryID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, os.ErrNotExist
	}
	if err != nil {
		return 0, fmt.Errorf("query WebDAV quota root: %w", err)
	}
	return entryID, nil
}

func enforceWebDAVQuotaTx(
	ctx context.Context,
	queryer database.IQueryer,
	root string,
	limit int64,
	current *entity.FileLinkMeta,
	newFileID uint64,
	newSize int64,
) error {
	if limit <= 0 {
		return nil
	}
	used, err := queryWebDAVQuotaUsed(ctx, queryer, root)
	if err != nil {
		return err
	}
	projected := used
	newReferenced, err := webDAVFileReferencedUnderRoot(ctx, queryer, root, newFileID)
	if err != nil {
		return err
	}
	if !newReferenced {
		projected += newSize
	}
	if current != nil && !current.IsDir && current.FileId != newFileID {
		oldReferenceCount, err := webDAVFileReferenceCountUnderRoot(
			ctx,
			queryer,
			root,
			current.FileId,
		)
		if err != nil {
			return err
		}
		if oldReferenceCount == 1 {
			projected -= current.FileSize
		}
	}
	if projected > limit {
		return ErrWebDAVQuota
	}
	return nil
}

func webDAVFileReferencedUnderRoot(
	ctx context.Context,
	queryer database.IQueryer,
	root string,
	fileID uint64,
) (bool, error) {
	count, err := webDAVFileReferenceCountUnderRoot(ctx, queryer, root, fileID)
	return count != 0, err
}

func webDAVFileReferenceCountUnderRoot(
	ctx context.Context,
	queryer database.IQueryer,
	root string,
	fileID uint64,
) (int, error) {
	entryID, err := queryWebDAVRootEntryID(ctx, queryer, root)
	if err != nil {
		return 0, err
	}
	var count int
	if err := queryRow(
		ctx,
		queryer,
		`WITH RECURSIVE subtree(entry_id, ref_data, file_kind) AS (
SELECT entry_id, ref_data, file_kind
FROM tg_file_mapping_tab WHERE entry_id = ?
UNION ALL
SELECT child.entry_id, child.ref_data, child.file_kind
FROM tg_file_mapping_tab child
JOIN subtree parent ON child.parent_entry_id = parent.entry_id
)
SELECT COUNT(*) FROM subtree WHERE file_kind = 2 AND ref_data = ?`,
		entryID,
		strconv.FormatUint(fileID, 10),
	).Scan(&count); err != nil {
		return 0, fmt.Errorf("count WebDAV file references: %w", err)
	}
	return count, nil
}

func webDAVChangeInScope(root, changedPath, depth string) bool {
	root = path.Clean(root)
	changedPath = path.Clean(changedPath)
	if changedPath == root {
		return true
	}
	if root != "/" && !strings.HasPrefix(changedPath, root+"/") {
		return false
	}
	if root == "/" && !strings.HasPrefix(changedPath, "/") {
		return false
	}
	if depth == webDAVLockDepthInfinity {
		return true
	}
	relative := strings.TrimPrefix(changedPath, root)
	relative = strings.TrimPrefix(relative, "/")
	return relative != "" && !strings.Contains(relative, "/")
}

func boolToInteger(value bool) int {
	if value {
		return 1
	}
	return 0
}
