package directory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	"github.com/didi/gendry/builder"
	"github.com/xxxsen/common/database"
	"github.com/xxxsen/common/database/dbkit"
)

var (
	ErrInvalidPath               = errors.New("invalid directory path")
	ErrPathComponentNotDirectory = errors.New("path component is not a directory")
	ErrSourceNotFound            = errors.New("source path not found")
	ErrDestinationMustNotBeRoot  = errors.New("destination must not be root")
	ErrDestinationExists         = errors.New("destination exists and overwrite is disabled")
	ErrDestinationInsideSource   = errors.New("destination is inside source")
	ErrEntryMustNotBeRoot        = errors.New("file entry must not be root")
	ErrEntryNotFile              = errors.New("directory entry is not a file")
	ErrParentNotFound            = errors.New("parent collection does not exist")

	errNoRowsAffected          = errors.New("no rows affected")
	errNoRowsInserted          = errors.New("no rows inserted")
	errRootNotFoundAfterCreate = errors.New("root entry not found after creation")
	errInvalidScanBatch        = errors.New("invalid directory scan batch size")
)

type IDGenFunc func() uint64

type onSelectDirFunc func(ctx context.Context, parentid uint64, tx database.IQueryExecer) error

type dbDirectory struct {
	db   database.IDatabase
	tab  string
	idfn IDGenFunc
}

type directoryTransaction struct {
	directory *dbDirectory
	tx        database.IQueryExecer
}

func (t *directoryTransaction) QueryExecer() database.IQueryExecer {
	return t.tx
}

func (t *directoryTransaction) Stat(
	ctx context.Context,
	filename string,
) (IDirectoryEntry, bool, error) {
	entry, exists, err := t.directory.txGetEntryInfo(ctx, t.tx, filename, nil)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("stat transaction path %q: %w", filename, err)
	}
	if !exists {
		return nil, false, nil
	}
	return entry.ToDirectoyEntry(), true, nil
}

func (t *directoryTransaction) Create(
	ctx context.Context,
	filename string,
	size int64,
	refdata string,
) (IDirectoryEntry, error) {
	filename = strings.TrimSuffix(filename, "/")
	dir, name, isRoot := t.directory.splitFilename(filename)
	if isRoot {
		return nil, ErrEntryMustNotBeRoot
	}
	var created IDirectoryEntry
	err := t.directory.txOnSelectDir(ctx, t.tx, dir, true, func(
		ctx context.Context,
		parentID uint64,
		tx database.IQueryExecer,
	) error {
		exists, err := t.directory.txIsEntryExist(ctx, tx, parentID, name)
		if err != nil {
			return fmt.Errorf("check file entry %q: %w", name, err)
		}
		if exists {
			return os.ErrExist
		}
		now := time.Now().UnixMilli()
		entry := &directoryEntryTab{
			ParentEntryId_: parentID,
			RefData_:       refdata,
			FileKind_:      defaultFileKindFile,
			Ctime_:         now,
			Mtime_:         now,
			FileSize_:      size,
			FileMode_:      defaultEntryFileMode,
			FileName_:      name,
		}
		entryID, err := t.directory.txCreateFile(ctx, tx, parentID, entry)
		if err != nil {
			return fmt.Errorf("create file entry %q: %w", name, err)
		}
		entry.EntryId_ = entryID
		created = entry
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("create transaction path %q: %w", filename, err)
	}
	if err := t.directory.recordChange(ctx, t.tx, filename, "created"); err != nil {
		return nil, err
	}
	if err := t.directory.touchParent(ctx, t.tx, filename, time.Now().UnixMilli()); err != nil {
		return nil, err
	}
	return created, nil
}

func (t *directoryTransaction) Mkdir(
	ctx context.Context,
	dirname string,
) (IDirectoryEntry, error) {
	dirname = strings.TrimSuffix(dirname, "/")
	parent, name, isRoot := t.directory.splitFilename(dirname)
	if isRoot {
		return nil, ErrEntryMustNotBeRoot
	}
	var created IDirectoryEntry
	err := t.directory.txOnSelectDir(ctx, t.tx, parent, false, func(
		ctx context.Context,
		parentID uint64,
		tx database.IQueryExecer,
	) error {
		if exists, err := t.directory.txIsEntryExist(ctx, tx, parentID, name); err != nil {
			return fmt.Errorf("check directory entry %q: %w", name, err)
		} else if exists {
			return os.ErrExist
		}
		entryID, err := t.directory.txCreateDir(ctx, tx, parentID, name)
		if err != nil {
			return fmt.Errorf("create directory entry %q: %w", name, err)
		}
		entry, exists, err := t.directory.txSearchEntry(ctx, tx, parentID, name)
		if err != nil || !exists {
			return fmt.Errorf("read created directory entry %q: %w", name, err)
		}
		entry.EntryId_ = entryID
		created = entry
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrParentNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("create transaction directory %q: %w", dirname, err)
	}
	if err := t.directory.recordChange(ctx, t.tx, dirname, "created"); err != nil {
		return nil, err
	}
	if err := t.directory.touchParent(ctx, t.tx, dirname, time.Now().UnixMilli()); err != nil {
		return nil, err
	}
	return created, nil
}

func (t *directoryTransaction) Replace(
	ctx context.Context,
	filename string,
	size int64,
	refdata string,
	mtime int64,
) (IDirectoryEntry, error) {
	entry, exists, err := t.directory.txGetEntryInfo(ctx, t.tx, filename, nil)
	if err != nil {
		return nil, fmt.Errorf("find transaction replace path %q: %w", filename, err)
	}
	if !exists {
		return nil, os.ErrNotExist
	}
	if entry.FileKind_ != defaultFileKindFile {
		return nil, ErrEntryNotFile
	}
	statement, args, err := builder.BuildUpdate(t.directory.table(), map[string]any{
		"entry_id": entry.EntryId_,
	}, map[string]any{
		"ref_data":  refdata,
		"file_size": size,
		"mtime":     mtime,
	})
	if err != nil {
		return nil, fmt.Errorf("build transaction replace: %w", err)
	}
	if _, err := t.tx.ExecContext(ctx, statement, args...); err != nil {
		return nil, fmt.Errorf("replace transaction entry: %w", err)
	}
	previous := entry.ToDirectoyEntry()
	if err := t.directory.recordChange(ctx, t.tx, filename, "updated"); err != nil {
		return nil, err
	}
	if err := t.directory.touchParent(ctx, t.tx, filename, mtime); err != nil {
		return nil, err
	}
	entry.RefData_ = refdata
	entry.FileSize_ = size
	entry.Mtime_ = mtime
	return previous, nil
}

func (t *directoryTransaction) Remove(
	ctx context.Context,
	filename string,
) ([]IDirectoryEntry, error) {
	entry, exists, err := t.directory.txGetEntryInfo(ctx, t.tx, filename, nil)
	if err != nil {
		return nil, fmt.Errorf("find transaction remove path %q: %w", filename, err)
	}
	if !exists {
		return nil, nil
	}
	removed := make([]IDirectoryEntry, 0, 1)
	if err := t.collectAndRemove(ctx, path.Clean(filename), entry, &removed); err != nil {
		return nil, err
	}
	if err := t.directory.touchParent(ctx, t.tx, filename, time.Now().UnixMilli()); err != nil {
		return nil, err
	}
	return removed, nil
}

func (t *directoryTransaction) collectAndRemove(
	ctx context.Context,
	entryPath string,
	entry *directoryEntryTab,
	removed *[]IDirectoryEntry,
) error {
	if entry.FileKind_ == defaultFileKindDir {
		children, err := t.directory.txListAllDir(ctx, t.tx, entry.EntryId_)
		if err != nil {
			return fmt.Errorf("list transaction remove children: %w", err)
		}
		for _, child := range children {
			if err := t.collectAndRemove(
				ctx,
				path.Join(entryPath, child.FileName_),
				child,
				removed,
			); err != nil {
				return err
			}
		}
	}
	*removed = append(*removed, entry)
	if err := t.directory.recordChange(ctx, t.tx, entryPath, "deleted"); err != nil {
		return err
	}
	if err := t.directory.txRemove(ctx, t.tx, entry.ParentEntryId_, entry.FileName_); err != nil {
		return fmt.Errorf("remove transaction entry %q: %w", entry.FileName_, err)
	}
	return nil
}

func (t *directoryTransaction) Touch(ctx context.Context, filename string, mtime int64) error {
	entry, exists, err := t.directory.txGetEntryInfo(ctx, t.tx, filename, nil)
	if err != nil {
		return fmt.Errorf("find transaction touch path %q: %w", filename, err)
	}
	if !exists {
		return os.ErrNotExist
	}
	sql, args, err := builder.BuildUpdate(t.directory.table(), map[string]any{
		"entry_id": entry.EntryId_,
	}, map[string]any{
		"mtime": mtime,
	})
	if err != nil {
		return fmt.Errorf("build transaction touch: %w", err)
	}
	if _, err := t.tx.ExecContext(ctx, sql, args...); err != nil {
		return fmt.Errorf("touch transaction entry: %w", err)
	}
	return t.directory.recordChange(ctx, t.tx, filename, "updated")
}

func (t *directoryTransaction) Copy(
	ctx context.Context,
	source, destination string,
	overwrite bool,
) ([]EntryCopy, []IDirectoryEntry, error) {
	return t.CopyDepth(ctx, source, destination, overwrite, true)
}

func (t *directoryTransaction) CopyDepth(
	ctx context.Context,
	source, destination string,
	overwrite, recursive bool,
) ([]EntryCopy, []IDirectoryEntry, error) {
	next, err := t.directory.precheckMoveCopy(source, destination)
	if err != nil {
		return nil, nil, err
	}
	if !next {
		return nil, nil, nil
	}
	var overwritten []IDirectoryEntry
	destinationEntry, destinationExists, err := t.directory.txGetEntryInfo(ctx, t.tx, destination, nil)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, nil, err
	}
	if destinationExists {
		if err := t.collectEntries(ctx, destinationEntry, &overwritten); err != nil {
			return nil, nil, err
		}
	}
	if err := t.directory.txDoCopyDepth(
		ctx,
		t.tx,
		source,
		destination,
		overwrite,
		recursive,
	); err != nil {
		return nil, nil, err
	}
	sourceEntry, sourceExists, err := t.directory.txGetEntryInfo(ctx, t.tx, source, nil)
	if err != nil || !sourceExists {
		return nil, nil, fmt.Errorf("read copied source: %w", err)
	}
	copiedEntry, copiedExists, err := t.directory.txGetEntryInfo(ctx, t.tx, destination, nil)
	if err != nil || !copiedExists {
		return nil, nil, fmt.Errorf("read copied destination: %w", err)
	}
	pairs := make([]EntryCopy, 0, 1)
	if err := t.collectCopyPairs(ctx, sourceEntry, copiedEntry, recursive, &pairs); err != nil {
		return nil, nil, err
	}
	return pairs, overwritten, nil
}

func (t *directoryTransaction) collectEntries(
	ctx context.Context,
	entry *directoryEntryTab,
	entries *[]IDirectoryEntry,
) error {
	if entry.FileKind_ == defaultFileKindDir {
		children, err := t.directory.txListAllDir(ctx, t.tx, entry.EntryId_)
		if err != nil {
			return fmt.Errorf("list transaction entry children: %w", err)
		}
		for _, child := range children {
			if err := t.collectEntries(ctx, child, entries); err != nil {
				return err
			}
		}
	}
	*entries = append(*entries, entry.ToDirectoyEntry())
	return nil
}

func (t *directoryTransaction) collectCopyPairs(
	ctx context.Context,
	source, destination *directoryEntryTab,
	recursive bool,
	pairs *[]EntryCopy,
) error {
	*pairs = append(*pairs, EntryCopy{
		Source:      source.ToDirectoyEntry(),
		Destination: destination.ToDirectoyEntry(),
	})
	if source.FileKind_ == defaultFileKindFile || !recursive {
		return nil
	}
	sourceChildren, err := t.directory.txListAllDir(ctx, t.tx, source.EntryId_)
	if err != nil {
		return err
	}
	for _, sourceChild := range sourceChildren {
		destinationChild, exists, err := t.directory.txSearchEntry(
			ctx,
			t.tx,
			destination.EntryId_,
			sourceChild.FileName_,
		)
		if err != nil || !exists {
			return fmt.Errorf("find copied child %q: %w", sourceChild.FileName_, err)
		}
		if err := t.collectCopyPairs(ctx, sourceChild, destinationChild, true, pairs); err != nil {
			return err
		}
	}
	return nil
}

func (t *directoryTransaction) Move(
	ctx context.Context,
	source, destination string,
	overwrite bool,
) ([]IDirectoryEntry, error) {
	next, err := t.directory.precheckMoveCopy(source, destination)
	if err != nil {
		return nil, err
	}
	if !next {
		return nil, nil
	}
	var overwritten []IDirectoryEntry
	destinationEntry, destinationExists, err := t.directory.txGetEntryInfo(ctx, t.tx, destination, nil)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if destinationExists {
		if err := t.collectEntries(ctx, destinationEntry, &overwritten); err != nil {
			return nil, err
		}
	}
	if err := t.directory.recordTreeChanges(ctx, t.tx, source, "deleted"); err != nil {
		return nil, err
	}
	if err := t.directory.txDoMove(ctx, t.tx, source, destination, overwrite); err != nil {
		return nil, err
	}
	if err := t.directory.recordTreeChanges(ctx, t.tx, destination, "created"); err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	if err := t.directory.touchParent(ctx, t.tx, source, now); err != nil {
		return nil, err
	}
	if err := t.directory.touchParent(ctx, t.tx, destination, now); err != nil {
		return nil, err
	}
	return overwritten, nil
}

func (e *dbDirectory) WithTransaction(ctx context.Context, callback TransactionFunc) error {
	if err := e.db.OnTransation(ctx, func(ctx context.Context, tx database.IQueryExecer) error {
		return callback(ctx, &directoryTransaction{directory: e, tx: tx})
	}); err != nil {
		return fmt.Errorf("directory transaction: %w", err)
	}
	return nil
}

func (e *dbDirectory) isArrayEqual(origin, ck []string) bool {
	if len(origin) != len(ck) {
		return false
	}
	for i, item := range origin {
		if item != ck[i] {
			return false
		}
	}
	return true
}

func (e *dbDirectory) isArrayHasSuffix(origin, prefix []string) bool {
	if len(origin) < len(prefix) {
		return false
	}
	for i, item := range prefix {
		if origin[i] != item {
			return false
		}
	}
	return true
}

func (e *dbDirectory) rebuildDirItems(dir string) ([]string, error) {
	items := strings.Split(dir, "/")
	rs := make([]string, 0, len(items)+1)
	rs = append(rs, "/")
	for _, item := range items {
		if len(item) == 0 || item == "." {
			continue
		}
		if item == ".." {
			return nil, ErrInvalidPath
		}
		rs = append(rs, item)
	}
	return rs, nil
}

func (e *dbDirectory) table() string {
	return e.tab
}

func (e *dbDirectory) recordChange(
	ctx context.Context,
	exec database.IExecer,
	entryPath, kind string,
) error {
	entryPath = path.Clean(entryPath)
	if _, err := exec.ExecContext(
		ctx,
		`INSERT INTO tg_webdav_change_tab(path, change_kind, changed_at)
VALUES (?, ?, ?)`,
		entryPath,
		kind,
		time.Now().UnixMilli(),
	); err != nil {
		return fmt.Errorf("record directory change: %w", err)
	}
	return nil
}

func (e *dbDirectory) touchParent(
	ctx context.Context,
	exec database.IQueryExecer,
	entryPath string,
	mtime int64,
) error {
	parentPath := path.Dir(path.Clean(entryPath))
	parent, exists, err := e.txGetEntryInfo(ctx, exec, parentPath, nil)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("find parent collection %q: %w", parentPath, err)
	}
	if !exists {
		return nil
	}
	statement, args, err := builder.BuildUpdate(e.table(), map[string]any{
		"entry_id": parent.EntryId_,
	}, map[string]any{
		"mtime": mtime,
	})
	if err != nil {
		return fmt.Errorf("build parent collection touch: %w", err)
	}
	if _, err := exec.ExecContext(ctx, statement, args...); err != nil {
		return fmt.Errorf("touch parent collection: %w", err)
	}
	return nil
}

func (e *dbDirectory) touchEntryID(
	ctx context.Context,
	exec database.IExecer,
	entryID uint64,
	mtime int64,
) error {
	statement, args, err := builder.BuildUpdate(e.table(), map[string]any{
		"entry_id": entryID,
	}, map[string]any{
		"mtime": mtime,
	})
	if err != nil {
		return fmt.Errorf("build collection touch: %w", err)
	}
	if _, err := exec.ExecContext(ctx, statement, args...); err != nil {
		return fmt.Errorf("touch collection: %w", err)
	}
	return nil
}

func (e *dbDirectory) recordTreeChanges(
	ctx context.Context,
	queryExecer database.IQueryExecer,
	root, kind string,
) error {
	entry, exists, err := e.txGetEntryInfo(ctx, queryExecer, root, nil)
	if err != nil {
		return fmt.Errorf("find changed tree root %q: %w", root, err)
	}
	if !exists {
		return os.ErrNotExist
	}
	var walk func(string, *directoryEntryTab) error
	walk = func(entryPath string, current *directoryEntryTab) error {
		if err := e.recordChange(ctx, queryExecer, entryPath, kind); err != nil {
			return err
		}
		if current.FileKind_ != defaultFileKindDir {
			return nil
		}
		children, err := e.txListAllDir(ctx, queryExecer, current.EntryId_)
		if err != nil {
			return fmt.Errorf("list changed tree %q: %w", entryPath, err)
		}
		for _, child := range children {
			if err := walk(path.Join(entryPath, child.FileName_), child); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(path.Clean(root), entry)
}

func (e *dbDirectory) splitFilename(filename string) (string, string, bool) {
	filename = path.Clean(filename)
	if filename == "/" {
		return "", "/", true
	}
	filename = strings.TrimSuffix(filename, "/")
	dir := path.Dir(filename)
	name := path.Base(filename)
	return dir, name, false
}

func (e *dbDirectory) txSearchEntry(
	ctx context.Context,
	q database.IQueryer,
	pid uint64,
	name string,
) (*directoryEntryTab, bool, error) {
	where := map[string]any{
		"parent_entry_id": pid,
		"file_name":       name,
		"_limit":          []uint{0, 1},
	}
	rs := make([]*directoryEntryTab, 0, 1)
	if err := dbkit.SimpleQuery(ctx, q, e.table(), where, &rs, dbkit.ScanWithTagName("json")); err != nil {
		return nil, false, fmt.Errorf("query directory entry %q: %w", name, err)
	}
	if len(rs) == 0 {
		return nil, false, nil
	}
	return rs[0], true, nil
}

func (e *dbDirectory) txChangeParent(
	ctx context.Context,
	exec database.IExecer,
	entryid, parentid uint64,
	newname *string,
) error {
	where := map[string]any{
		"entry_id": entryid,
	}
	update := map[string]any{
		"parent_entry_id": parentid,
	}
	if newname != nil {
		update["file_name"] = *newname
	}
	sql, args, err := builder.BuildUpdate(e.table(), where, update)
	if err != nil {
		return fmt.Errorf("build directory parent update: %w", err)
	}
	rs, err := exec.ExecContext(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("update directory parent: %w", err)
	}
	cnt, err := rs.RowsAffected()
	if err != nil {
		return fmt.Errorf("read directory parent update count: %w", err)
	}
	if cnt == 0 {
		return errNoRowsAffected
	}
	return nil
}

func (e *dbDirectory) txIsEntryExist(ctx context.Context, q database.IQueryer, pid uint64, name string) (bool, error) {
	_, ok, err := e.txSearchEntry(ctx, q, pid, name)
	if err != nil {
		return false, err
	}
	return ok, nil
}

func (e *dbDirectory) newEntryId() uint64 {
	return e.idfn()
}

func (e *dbDirectory) txCreateEntry(
	ctx context.Context,
	exec database.IExecer,
	pid uint64,
	ent *directoryEntryTab,
) (uint64, error) {
	eid := e.newEntryId()
	data := []map[string]any{
		{
			"entry_id":        eid,
			"parent_entry_id": pid,
			"ref_data":        ent.RefData_,
			"file_kind":       ent.FileKind_,
			"ctime":           ent.Ctime_,
			"mtime":           ent.Mtime_,
			"file_size":       ent.FileSize_,
			"file_mode":       ent.FileMode_,
			"file_name":       ent.FileName_,
		},
	}
	sql, args, err := builder.BuildInsert(e.table(), data)
	if err != nil {
		return 0, fmt.Errorf("build directory entry insert: %w", err)
	}
	rs, err := exec.ExecContext(ctx, sql, args...)
	if err != nil {
		return 0, fmt.Errorf("insert directory entry: %w", err)
	}
	cnt, err := rs.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read directory entry insert count: %w", err)
	}
	if cnt == 0 {
		return 0, errNoRowsInserted
	}
	return eid, nil
}

func (e *dbDirectory) txRemove(ctx context.Context, tx database.IExecer, parentid uint64, name string) error {
	where := map[string]any{
		"parent_entry_id": parentid,
		"file_name":       name,
	}
	sql, args, err := builder.BuildDelete(e.table(), where)
	if err != nil {
		return fmt.Errorf("build directory entry delete: %w", err)
	}
	if _, err := tx.ExecContext(ctx, sql, args...); err != nil {
		return fmt.Errorf("delete directory entry: %w", err)
	}
	return nil
}

func (e *dbDirectory) txCreateDir(ctx context.Context, exec database.IExecer, pid uint64, name string) (uint64, error) {
	now := time.Now().UnixMilli()
	ent := &directoryEntryTab{
		ParentEntryId_: pid,
		RefData_:       "",
		FileKind_:      defaultFileKindDir,
		Ctime_:         now,
		Mtime_:         now,
		FileSize_:      0,
		FileMode_:      defaultEntryFileMode,
		FileName_:      name,
	}
	return e.txCreateEntry(ctx, exec, pid, ent)
}

func (e *dbDirectory) txCreateFile(
	ctx context.Context,
	exec database.IExecer,
	pid uint64,
	ent *directoryEntryTab,
) (uint64, error) {
	return e.txCreateEntry(ctx, exec, pid, ent)
}

func (e *dbDirectory) txListDir(
	ctx context.Context,
	q database.IQueryer,
	parentid uint64,
	offset, limit uint,
) ([]*directoryEntryTab, error) {
	where := map[string]any{
		"parent_entry_id": parentid,
		"_orderby":        "entry_id ASC",
		"_limit":          []uint{offset, limit},
	}
	rs := make([]*directoryEntryTab, 0, limit)
	if err := dbkit.SimpleQuery(ctx, q, e.table(), where, &rs, dbkit.ScanWithTagName("json")); err != nil {
		return nil, fmt.Errorf("list directory entries: %w", err)
	}
	return rs, nil
}

func (e *dbDirectory) txListDirAfter(
	ctx context.Context,
	q database.IQueryer,
	parentID, lastEntryID uint64,
	limit uint,
) ([]*directoryEntryTab, error) {
	where := map[string]any{
		"parent_entry_id": parentID,
		"entry_id >":      lastEntryID,
		"_orderby":        "entry_id ASC",
		"_limit":          []uint{0, limit},
	}
	entries := make([]*directoryEntryTab, 0, limit)
	if err := dbkit.SimpleQuery(
		ctx,
		q,
		e.table(),
		where,
		&entries,
		dbkit.ScanWithTagName("json"),
	); err != nil {
		return nil, fmt.Errorf("list directory entries after cursor: %w", err)
	}
	return entries, nil
}

func (e *dbDirectory) txListAllDir(
	ctx context.Context,
	q database.IQueryExecer,
	parentid uint64,
) ([]*directoryEntryTab, error) {
	var offset uint
	const limit uint = 128
	rs := make([]*directoryEntryTab, 0, limit)
	for offset = 0; ; offset += limit {
		ents, err := e.txListDir(ctx, q, parentid, offset, limit)
		if err != nil {
			return nil, err
		}
		rs = append(rs, ents...)
		if uint(len(ents)) < limit {
			break
		}
	}
	return rs, nil
}

func (e *dbDirectory) txOnSelectDir(
	ctx context.Context,
	tx database.IQueryExecer,
	dir string,
	allowCreate bool,
	cb onSelectDirFunc,
) error {
	// 逐级查找并创建目录, 返回最后的目录的id
	items, err := e.rebuildDirItems(dir)
	if err != nil {
		return fmt.Errorf("normalize directory path %q: %w", dir, err)
	}
	var parentid uint64
	for idx, item := range items {
		ent, err := e.txSelectDirComponent(
			ctx,
			tx,
			parentid,
			item,
			path.Join(items[:idx+1]...),
			allowCreate,
		)
		if err != nil {
			return err
		}
		if ent.FileKind_ != defaultFileKindDir {
			return fmt.Errorf(
				"%w: %s",
				ErrPathComponentNotDirectory,
				strings.Join(items[:idx+1], "/"),
			)
		}
		parentid = ent.EntryId_
	}
	return cb(ctx, parentid, tx)
}

func (e *dbDirectory) txSelectDirComponent(
	ctx context.Context,
	tx database.IQueryExecer,
	parentID uint64,
	name, componentPath string,
	allowCreate bool,
) (*directoryEntryTab, error) {
	entry, exists, err := e.txSearchEntry(ctx, tx, parentID, name)
	if err != nil {
		return nil, fmt.Errorf("search path component %q: %w", name, err)
	}
	if exists {
		return entry, nil
	}
	if !allowCreate {
		return nil, os.ErrNotExist
	}
	entryID, err := e.txCreateDir(ctx, tx, parentID, name)
	if err != nil {
		return nil, fmt.Errorf("create path component %q: %w", name, err)
	}
	if err := e.recordChange(ctx, tx, componentPath, "created"); err != nil {
		return nil, err
	}
	if parentID != 0 {
		if err := e.touchEntryID(ctx, tx, parentID, time.Now().UnixMilli()); err != nil {
			return nil, err
		}
	}
	return &directoryEntryTab{
		EntryId_:  entryID,
		FileKind_: defaultFileKindDir,
		FileName_: name,
	}, nil
}

func (e *dbDirectory) txGetRoot(ctx context.Context, tx database.IQueryExecer) (*directoryEntryTab, bool, error) {
	return e.txSearchEntry(ctx, tx, 0, "/")
}

func (e *dbDirectory) txCreateRoot(ctx context.Context, tx database.IQueryExecer) (*directoryEntryTab, error) {
	ent, ok, err := e.txGetRoot(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("get root entry: %w", err)
	}
	if ok {
		return ent, nil
	}
	now := time.Now().UnixMilli()
	_, err = e.txCreateEntry(ctx, tx, 0, &directoryEntryTab{
		ParentEntryId_: 0,
		RefData_:       "",
		FileKind_:      defaultFileKindDir,
		Ctime_:         now,
		Mtime_:         now,
		FileSize_:      0,
		FileMode_:      defaultEntryFileMode,
		FileName_:      "/",
	})
	if err != nil {
		return nil, fmt.Errorf("create root entry: %w", err)
	}
	ent, ok, err = e.txGetRoot(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("get created root entry: %w", err)
	}
	if !ok {
		return nil, errRootNotFoundAfterCreate
	}
	return ent, nil
}

func (e *dbDirectory) txGetEntryInfo(
	ctx context.Context,
	tx database.IQueryExecer,
	location string,
	exportLastParentID *uint64,
) (*directoryEntryTab, bool, error) {
	dir, name, isRoot := e.splitFilename(location)
	if isRoot {
		return e.txGetRoot(ctx, tx)
	}
	var sinfo *directoryEntryTab
	var exist bool
	onSelect := func(ctx context.Context, parentid uint64, tx database.IQueryExecer) error {
		if exportLastParentID != nil {
			*exportLastParentID = parentid
		}
		ent, ok, err := e.txSearchEntry(ctx, tx, parentid, name)
		if err != nil {
			return fmt.Errorf("search directory entry %q: %w", name, err)
		}
		if !ok {
			return nil
		}
		sinfo = ent
		exist = true
		return nil
	}
	if err := e.txOnSelectDir(ctx, tx, dir, false, onSelect); err != nil {
		return nil, false, fmt.Errorf("find directory entry %q: %w", location, err)
	}
	if !exist {
		return nil, false, nil
	}
	return sinfo, true, nil
}

func (e *dbDirectory) onSelectDir(ctx context.Context, dir string, allowCreate bool, cb onSelectDirFunc) error {
	err := e.db.OnTransation(ctx, func(ctx context.Context, qe database.IQueryExecer) error {
		return e.txOnSelectDir(ctx, qe, dir, allowCreate, cb)
	})
	if err != nil {
		return fmt.Errorf("select directory %q: %w", dir, err)
	}
	return nil
}

func (e *dbDirectory) Mkdir(ctx context.Context, dir string) error {
	pdir, name, isRoot := e.splitFilename(dir)
	if isRoot {
		if _, err := e.txCreateRoot(ctx, e.db); err != nil {
			return fmt.Errorf("create root directory: %w", err)
		}
		return nil
	}

	if err := e.onSelectDir(ctx, pdir, true, func(ctx context.Context, parentid uint64, tx database.IQueryExecer) error {
		exist, err := e.txIsEntryExist(ctx, tx, parentid, name)
		if err != nil {
			return fmt.Errorf("check directory %q: %w", name, err)
		}
		if exist {
			return nil
		}
		if _, err := e.txCreateDir(ctx, tx, parentid, name); err != nil {
			return fmt.Errorf("create directory %q: %w", name, err)
		}
		if err := e.recordChange(ctx, tx, dir, "created"); err != nil {
			return err
		}
		return e.touchEntryID(ctx, tx, parentid, time.Now().UnixMilli())
	}); err != nil {
		return fmt.Errorf("make directory %q: %w", dir, err)
	}
	return nil
}

func (e *dbDirectory) txDoIterAndCopy(
	ctx context.Context,
	tx database.IQueryExecer,
	srcinfo *directoryEntryTab,
	dstparent uint64,
	destination string,
	newname string,
	recursive bool,
) error {
	now := time.Now().UnixMilli()
	dstentid, err := e.txCreateEntry(ctx, tx, dstparent, &directoryEntryTab{
		ParentEntryId_: dstparent,
		RefData_:       srcinfo.RefData_,
		FileKind_:      srcinfo.FileKind_,
		Ctime_:         now,
		Mtime_:         now,
		FileSize_:      srcinfo.FileSize_,
		FileMode_:      srcinfo.FileMode_,
		FileName_:      newname,
	})
	if err != nil {
		return fmt.Errorf("create copied entry %q: %w", newname, err)
	}
	if err := e.recordChange(ctx, tx, destination, "created"); err != nil {
		return err
	}
	if srcinfo.FileKind_ == defaultFileKindFile || !recursive {
		return nil
	}
	items, err := e.txListAllDir(ctx, tx, srcinfo.EntryId_)
	if err != nil {
		return fmt.Errorf("list all dir failed, eid:%d, err:%w", srcinfo.EntryId_, err)
	}
	for _, item := range items { // 递归创建子节点
		if err := e.txDoIterAndCopy(
			ctx,
			tx,
			item,
			dstentid,
			path.Join(destination, item.FileName_),
			item.FileName_,
			true,
		); err != nil {
			return fmt.Errorf("copy child entry %q: %w", item.FileName_, err)
		}
	}
	return nil
}

func (e *dbDirectory) txDoCopyDepth(
	ctx context.Context,
	tx database.IQueryExecer,
	src, dst string,
	overwrite, recursive bool,
) error {
	next, err := e.precheckMoveCopy(src, dst)
	if err != nil {
		return fmt.Errorf("precheck copy failed, err:%w", err)
	}
	if !next {
		return nil
	}

	sinfo, exist, err := e.txGetEntryInfo(ctx, tx, src, nil)
	if err != nil {
		return fmt.Errorf("get copy source %q: %w", src, err)
	}
	if !exist {
		return fmt.Errorf("%w: %s", ErrSourceNotFound, src)
	}
	var dstparentid uint64
	_, dname, isRoot := e.splitFilename(dst)
	if isRoot {
		return fmt.Errorf("%w: %s", ErrDestinationMustNotBeRoot, dst)
	}
	dinfo, exist, err := e.txGetEntryInfo(ctx, tx, dst, &dstparentid)
	if err != nil {
		return fmt.Errorf("get copy destination %q: %w", dst, err)
	}
	if exist {
		if !overwrite {
			return ErrDestinationExists
		}
		transaction := &directoryTransaction{directory: e, tx: tx}
		removed := make([]IDirectoryEntry, 0, 1)
		if err := transaction.collectAndRemove(ctx, path.Clean(dst), dinfo, &removed); err != nil {
			return fmt.Errorf("delete before copy: %w", err)
		}
	}
	// 执行递归copy流程
	if err := e.txDoIterAndCopy(
		ctx,
		tx,
		sinfo,
		dstparentid,
		path.Clean(dst),
		dname,
		recursive,
	); err != nil {
		return fmt.Errorf(
			"copy tree source_parent=%d destination_parent=%d name=%q: %w",
			sinfo.ParentEntryId_,
			dstparentid,
			sinfo.FileName_,
			err,
		)
	}
	return e.touchParent(ctx, tx, dst, time.Now().UnixMilli())
}

func (e *dbDirectory) Copy(ctx context.Context, src, dst string, overwrite bool) error {
	if err := e.WithTransaction(ctx, func(ctx context.Context, tx ITransaction) error {
		_, _, err := tx.Copy(ctx, src, dst, overwrite)
		if err != nil {
			return fmt.Errorf("copy directory transaction: %w", err)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("do copy failed, err:%w", err)
	}
	return nil
}

func (e *dbDirectory) precheckMoveCopy(src, dst string) (bool, error) {
	s, err := e.rebuildDirItems(src)
	if err != nil {
		return false, fmt.Errorf("normalize source path %q: %w", src, err)
	}
	d, err := e.rebuildDirItems(dst)
	if err != nil {
		return false, fmt.Errorf("normalize destination path %q: %w", dst, err)
	}
	if e.isArrayEqual(s, d) {
		return false, nil
	}
	if e.isArrayHasSuffix(d, s) {
		return false, ErrDestinationInsideSource
	}
	if e.isArrayHasSuffix(s, d) {
		return false, ErrDestinationInsideSource
	}
	return true, nil
}

func (e *dbDirectory) txDoMove(ctx context.Context, tx database.IQueryExecer, src, dst string, overwrite bool) error {
	// 将/a/b/1.txt 移动到目录/c下, 那么src = /a/b/1.txt, dst = /c/1.txt
	next, err := e.precheckMoveCopy(src, dst)
	if err != nil {
		return fmt.Errorf("pre check move failed, err:%w", err)
	}
	if !next {
		return nil
	}
	sinfo, ok, err := e.txGetEntryInfo(ctx, tx, src, nil)
	if err != nil {
		return fmt.Errorf("get move source %q: %w", src, err)
	}
	if !ok {
		return fmt.Errorf("%w: %s", ErrSourceNotFound, src)
	}
	// 处理move流程
	var parentid uint64
	dinfo, dexist, err := e.txGetEntryInfo(ctx, tx, dst, &parentid)
	if err != nil {
		return fmt.Errorf("get move destination %q: %w", dst, err)
	}
	_, dname, isRoot := e.splitFilename(dst)
	if isRoot {
		return ErrEntryMustNotBeRoot
	}
	if !dexist { // 目标不存在, 那么直接把src挂到dst的parent上即可
		if err := e.txChangeParent(ctx, tx, sinfo.EntryId_, parentid, &dname); err != nil {
			return fmt.Errorf("move entry to new parent: %w", err)
		}
		return nil
	}
	if !overwrite {
		return ErrDestinationExists
	}
	transaction := &directoryTransaction{directory: e, tx: tx}
	removed := make([]IDirectoryEntry, 0, 1)
	if err := transaction.collectAndRemove(ctx, path.Clean(dst), dinfo, &removed); err != nil {
		return fmt.Errorf("remove overwritten move destination: %w", err)
	}
	if err := e.txChangeParent(ctx, tx, sinfo.EntryId_, parentid, &dname); err != nil {
		return fmt.Errorf("move entry after replacing destination: %w", err)
	}
	return nil
}

func (e *dbDirectory) Move(ctx context.Context, src, dst string, overwrite bool) error {
	if err := e.WithTransaction(ctx, func(ctx context.Context, tx ITransaction) error {
		_, err := tx.Move(ctx, src, dst, overwrite)
		if err != nil {
			return fmt.Errorf("move directory transaction: %w", err)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("do move failed, err:%w", err)
	}
	return nil
}

func (e *dbDirectory) Remove(ctx context.Context, filename string) error {
	if err := e.WithTransaction(ctx, func(ctx context.Context, tx ITransaction) error {
		_, err := tx.Remove(ctx, filename)
		if err != nil {
			return fmt.Errorf("remove directory transaction: %w", err)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("remove path %q: %w", filename, err)
	}
	return nil
}

func (e *dbDirectory) Create(ctx context.Context, filename string, size int64, refdata string) error {
	if err := e.WithTransaction(ctx, func(ctx context.Context, tx ITransaction) error {
		_, err := tx.Create(ctx, filename, size, refdata)
		if err != nil {
			return fmt.Errorf("create directory transaction: %w", err)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("create file path %q: %w", filename, err)
	}
	return nil
}

func (e *dbDirectory) List(ctx context.Context, dir string) ([]IDirectoryEntry, error) {
	rs := make([]IDirectoryEntry, 0, 16)
	if err := e.onSelectDir(ctx, dir, false, func(ctx context.Context, parentid uint64, tx database.IQueryExecer) error {
		items, err := e.txListAllDir(ctx, tx, parentid)
		if err != nil {
			return fmt.Errorf("list directory entries: %w", err)
		}
		for _, item := range items {
			rs = append(rs, item.ToDirectoyEntry())
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("list directory %q: %w", dir, err)
	}
	return rs, nil
}

func (e *dbDirectory) Iterate(
	ctx context.Context,
	dir string,
	batch uint,
	cb DirectoryScanCallbackFunc,
) error {
	if batch == 0 {
		return errInvalidScanBatch
	}
	var parentID uint64
	if err := e.onSelectDir(
		ctx,
		dir,
		false,
		func(_ context.Context, resolvedParentID uint64, _ database.IQueryExecer) error {
			parentID = resolvedParentID
			return nil
		},
	); err != nil {
		return fmt.Errorf("iterate directory %q: %w", dir, err)
	}
	if err := e.txIterateDir(ctx, e.db, parentID, batch, cb); err != nil {
		return fmt.Errorf("iterate directory %q batches: %w", dir, err)
	}
	return nil
}

func (e *dbDirectory) txIterateDir(
	ctx context.Context,
	tx database.IQueryExecer,
	parentID uint64,
	batch uint,
	cb DirectoryScanCallbackFunc,
) error {
	var lastEntryID uint64
	for {
		items, err := e.txListDirAfter(ctx, tx, parentID, lastEntryID, batch)
		if err != nil {
			return err
		}
		entries := make([]IDirectoryEntry, 0, len(items))
		for _, item := range items {
			entries = append(entries, item.ToDirectoyEntry())
		}
		next, err := cb(ctx, entries)
		if err != nil {
			return fmt.Errorf("process directory batch: %w", err)
		}
		if !next || uint(len(items)) < batch {
			return nil
		}
		lastEntryID = items[len(items)-1].EntryId_
	}
}

func (e *dbDirectory) Stat(ctx context.Context, filename string) (IDirectoryEntry, error) {
	dir, name, isRoot := e.splitFilename(filename)
	if isRoot {
		ent, ok, err := e.txGetRoot(ctx, e.db)
		if err != nil {
			return nil, fmt.Errorf("stat root directory: %w", err)
		}
		if !ok {
			return nil, os.ErrNotExist
		}
		return ent.ToDirectoyEntry(), nil
	}
	var rs IDirectoryEntry
	if err := e.onSelectDir(ctx, dir, false, func(ctx context.Context, parentid uint64, tx database.IQueryExecer) error {
		t, ok, err := e.txSearchEntry(ctx, tx, parentid, name)
		if err != nil {
			return fmt.Errorf("stat directory entry %q: %w", name, err)
		}
		if !ok {
			return os.ErrNotExist
		}
		rs = t.ToDirectoyEntry()
		return nil
	}); err != nil {
		return nil, fmt.Errorf("stat path %q: %w", filename, err)
	}
	return rs, nil
}

func (e *dbDirectory) Scan(ctx context.Context, batch uint, cb DirectoryScanCallbackFunc) error {
	const maxScanBatch uint = 1_000_000
	if batch == 0 || batch > maxScanBatch {
		return fmt.Errorf("%w: %d", errInvalidScanBatch, batch)
	}
	var lastid uint64
	for {
		res, nextid, err := e.innerScan(ctx, lastid, batch)
		if err != nil {
			return fmt.Errorf("scan directory batch: %w", err)
		}
		lastid = nextid
		next, err := cb(ctx, res)
		if err != nil {
			return fmt.Errorf("process directory batch: %w", err)
		}
		if !next {
			break
		}
		if len(res) < int(batch) {
			break
		}
	}
	return nil
}

func (e *dbDirectory) innerScan(ctx context.Context, lastid uint64, batch uint) ([]IDirectoryEntry, uint64, error) {
	where := map[string]any{
		"id >":     lastid,
		"_orderby": "id asc",
		"_limit":   []uint{0, batch},
	}
	rs := make([]*directoryEntryTab, 0, batch)
	if err := dbkit.SimpleQuery(ctx, e.db, e.table(), where, &rs, dbkit.ScanWithTagName("json")); err != nil {
		return nil, 0, fmt.Errorf("query directory scan batch: %w", err)
	}
	if len(rs) == 0 {
		return nil, 0, nil
	}
	out := make([]IDirectoryEntry, 0, len(rs))
	for _, item := range rs {
		out = append(out, item.ToDirectoyEntry())
	}
	nextid := rs[len(rs)-1].Id_
	return out, nextid, nil
}

func NewDBDirectory(db database.IDatabase, tab string, idfn IDGenFunc) (ITransactionalDirectory, error) {
	return &dbDirectory{
		db:   db,
		tab:  tab,
		idfn: idfn,
	}, nil
}
