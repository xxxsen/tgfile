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
	errInvalidPath                  = errors.New("invalid directory path")
	errNoRowsAffected               = errors.New("no rows affected")
	errNoRowsInserted               = errors.New("no rows inserted")
	errRootNotFoundAfterCreate      = errors.New("root entry not found after creation")
	errPathComponentNotDirectory    = errors.New("path component is not a directory")
	errCopySourceNotFound           = errors.New("copy source not found")
	errDestinationMustNotBeRoot     = errors.New("destination must not be root")
	errCopyDestinationDirectory     = errors.New("copy destination directory already exists")
	errDestinationExists            = errors.New("destination exists and overwrite is disabled")
	errDestinationInsideSource      = errors.New("destination is inside source")
	errMoveSourceNotFound           = errors.New("move source not found")
	errDirectoryOverwriteDisallowed = errors.New("overwriting a directory is not allowed")
	errEntryMustNotBeRoot           = errors.New("file entry must not be root")
	errInvalidScanBatch             = errors.New("invalid directory scan batch size")
)

type IDGenFunc func() uint64

type onSelectDirFunc func(ctx context.Context, parentid uint64, tx database.IQueryExecer) error

type dbDirectory struct {
	db   database.IDatabase
	tab  string
	idfn IDGenFunc
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
			return nil, errInvalidPath
		}
		rs = append(rs, strings.TrimSpace(item))
	}
	return rs, nil
}

func (e *dbDirectory) table() string {
	return e.tab
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
		"_limit":          []uint{offset, limit},
	}
	rs := make([]*directoryEntryTab, 0, limit)
	if err := dbkit.SimpleQuery(ctx, q, e.table(), where, &rs, dbkit.ScanWithTagName("json")); err != nil {
		return nil, fmt.Errorf("list directory entries: %w", err)
	}
	return rs, nil
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
		ent, ok, err := e.txSearchEntry(ctx, tx, parentid, item)
		if err != nil {
			return fmt.Errorf("search path component %q: %w", item, err)
		}
		if !ok {
			if !allowCreate {
				return os.ErrNotExist
			}
			pid, err := e.txCreateDir(ctx, tx, parentid, item)
			if err != nil {
				return fmt.Errorf("create path component %q: %w", item, err)
			}
			parentid = pid
			continue
		}
		if ent.FileKind_ != defaultFileKindDir {
			return fmt.Errorf(
				"%w: %s",
				errPathComponentNotDirectory,
				strings.Join(items[:idx+1], "/"),
			)
		}
		parentid = ent.EntryId_
	}
	return cb(ctx, parentid, tx)
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
		return nil
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
	newname string,
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
	if srcinfo.FileKind_ == defaultFileKindFile { // 如果是文件, 则直接结束
		return nil
	}
	items, err := e.txListAllDir(ctx, tx, srcinfo.EntryId_)
	if err != nil {
		return fmt.Errorf("list all dir failed, eid:%d, err:%w", srcinfo.EntryId_, err)
	}
	for _, item := range items { // 递归创建子节点
		if err := e.txDoIterAndCopy(ctx, tx, item, dstentid, item.FileName_); err != nil {
			return fmt.Errorf("copy child entry %q: %w", item.FileName_, err)
		}
	}
	return nil
}

func (e *dbDirectory) txDoCopy(ctx context.Context, tx database.IQueryExecer, src, dst string, overwrite bool) error {
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
		return fmt.Errorf("%w: %s", errCopySourceNotFound, src)
	}
	var dstparentid uint64
	_, dname, isRoot := e.splitFilename(dst)
	if isRoot {
		return fmt.Errorf("%w: %s", errDestinationMustNotBeRoot, dst)
	}
	dinfo, exist, err := e.txGetEntryInfo(ctx, tx, dst, &dstparentid)
	if err != nil {
		return fmt.Errorf("get copy destination %q: %w", dst, err)
	}
	if exist {
		if dinfo.FileKind_ == defaultFileKindDir { // 存在且目标为目录, 直接跳过后续流程
			return errCopyDestinationDirectory
		}
		// 如果为文件, 则需要检查是否启用overwrite
		if !overwrite {
			return errDestinationExists
		}
		if err := e.txRemove(ctx, tx, dinfo.ParentEntryId_, dinfo.FileName_); err != nil {
			return fmt.Errorf("delete before copy failed, err:%w", err)
		}
	}
	// 执行递归copy流程
	if err := e.txDoIterAndCopy(ctx, tx, sinfo, dstparentid, dname); err != nil {
		return fmt.Errorf(
			"copy tree source_parent=%d destination_parent=%d name=%q: %w",
			sinfo.ParentEntryId_,
			dstparentid,
			sinfo.FileName_,
			err,
		)
	}
	return nil
}

func (e *dbDirectory) Copy(ctx context.Context, src, dst string, overwrite bool) error {
	if err := e.db.OnTransation(ctx, func(ctx context.Context, tx database.IQueryExecer) error {
		return e.txDoCopy(ctx, tx, src, dst, overwrite)
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
		return false, errDestinationInsideSource
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
		return fmt.Errorf("%w: %s", errMoveSourceNotFound, src)
	}
	// 处理move流程
	var parentid uint64
	dinfo, dexist, err := e.txGetEntryInfo(ctx, tx, dst, &parentid)
	if err != nil {
		return fmt.Errorf("get move destination %q: %w", dst, err)
	}
	_, dname, isRoot := e.splitFilename(dst)
	if isRoot {
		return errEntryMustNotBeRoot
	}
	if !dexist { // 目标不存在, 那么直接把src挂到dst的parent上即可
		if err := e.txChangeParent(ctx, tx, sinfo.EntryId_, parentid, &dname); err != nil {
			return fmt.Errorf("move entry to new parent: %w", err)
		}
		return nil
	}
	if dinfo.FileKind_ == defaultFileKindDir { // 不允许直接覆盖dir
		return errDirectoryOverwriteDisallowed
	}
	if !overwrite { // 文件存在, 但是又没又overwrite选项, 直接返回
		return errDestinationExists
	}
	// 删除老的, 并修改src父节点
	if err := e.txRemove(ctx, tx, parentid, dname); err != nil {
		return fmt.Errorf("overwrite but remove origin failed, err:%w", err)
	}
	if err := e.txChangeParent(ctx, tx, sinfo.EntryId_, parentid, &dname); err != nil {
		return fmt.Errorf("move entry after replacing destination: %w", err)
	}
	return nil
}

func (e *dbDirectory) Move(ctx context.Context, src, dst string, overwrite bool) error {
	if err := e.db.OnTransation(ctx, func(ctx context.Context, tx database.IQueryExecer) error {
		return e.txDoMove(ctx, tx, src, dst, overwrite)
	}); err != nil {
		return fmt.Errorf("do move failed, err:%w", err)
	}
	return nil
}

func (e *dbDirectory) txDoRemove(ctx context.Context, tx database.IQueryExecer, parentid uint64, name string) error {
	ent, ok, err := e.txSearchEntry(ctx, tx, parentid, name)
	if err != nil {
		return fmt.Errorf("read entry pid=%d name=%q: %w", parentid, name, err)
	}
	if !ok { // 已经被删了
		return nil
	}
	if ent.FileKind_ == defaultFileKindDir {
		items, err := e.txListAllDir(ctx, tx, ent.EntryId_)
		if err != nil {
			return fmt.Errorf("scan entry from pid:%d failed, err:%w", parentid, err)
		}
		for _, item := range items {
			if err := e.txDoRemove(ctx, tx, item.ParentEntryId_, item.FileName_); err != nil {
				return fmt.Errorf("remove child entry %q: %w", item.FileName_, err)
			}
		}
	}
	if err := e.txRemove(ctx, tx, parentid, name); err != nil {
		return fmt.Errorf("remove entry %q: %w", name, err)
	}
	return nil
}

func (e *dbDirectory) Remove(ctx context.Context, filename string) error {
	// 递归删除其子节点, 再删除父节点
	dir, name, isRoot := e.splitFilename(filename)
	if err := e.db.OnTransation(ctx, func(ctx context.Context, tx database.IQueryExecer) error {
		if isRoot {
			return e.txDoRemove(ctx, tx, 0, "/")
		}
		return e.txOnSelectDir(
			ctx,
			tx,
			dir,
			false,
			func(ctx context.Context, parentid uint64, tx database.IQueryExecer) error {
				return e.txDoRemove(ctx, tx, parentid, name)
			},
		)
	}); err != nil {
		return fmt.Errorf("remove path %q: %w", filename, err)
	}
	return nil
}

func (e *dbDirectory) Create(ctx context.Context, filename string, size int64, refdata string) error {
	filename = strings.TrimSuffix(filename, "/")
	dir, name, isRoot := e.splitFilename(filename)
	if isRoot {
		return errEntryMustNotBeRoot
	}
	if err := e.onSelectDir(ctx, dir, true, func(ctx context.Context, parentid uint64, tx database.IQueryExecer) error {
		exist, err := e.txIsEntryExist(ctx, tx, parentid, name)
		if err != nil {
			return fmt.Errorf("check file entry %q: %w", name, err)
		}
		if exist {
			return os.ErrExist
		}
		now := time.Now().UnixMilli()
		if _, err := e.txCreateFile(ctx, tx, parentid, &directoryEntryTab{
			RefData_:  refdata,
			FileKind_: 2,
			Ctime_:    now,
			Mtime_:    now,
			FileSize_: size,
			FileMode_: defaultEntryFileMode,
			FileName_: name,
		}); err != nil {
			return fmt.Errorf("create file entry %q: %w", name, err)
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

func NewDBDirectory(db database.IDatabase, tab string, idfn IDGenFunc) (IDirectory, error) {
	return &dbDirectory{
		db:   db,
		tab:  tab,
		idfn: idfn,
	}, nil
}
