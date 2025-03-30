package directory

import (
	"context"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/didi/gendry/builder"
	"github.com/xxxsen/common/database"
	"github.com/xxxsen/common/database/dbkit"
)

const (
	defaultMaxDepthLimit = 16
)

type IDGenFunc func() uint64

type onSelectDirFunc func(ctx context.Context, parentid uint64, tx database.IQueryExecer) error

type dbDirectory struct {
	db   database.IDatabase
	tab  string
	idfn IDGenFunc
}

func (e *dbDirectory) isArrayEqual(origin []string, ck []string) bool {
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

func (e *dbDirectory) isArrayHasSuffix(origin []string, prefix []string) bool {
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
			return nil, fmt.Errorf("invalid char in path")
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

func (e *dbDirectory) txSearchEntry(ctx context.Context, q database.IQueryer, pid uint64, name string) (*directoryEntryTab, bool, error) {
	where := map[string]interface{}{
		"parent_entry_id": pid,
		"file_name":       name,
		"_limit":          []uint{0, 1},
	}
	rs := make([]*directoryEntryTab, 0, 1)
	if err := dbkit.SimpleQuery(ctx, q, e.table(), where, &rs, dbkit.ScanWithTagName("json")); err != nil {
		return nil, false, err
	}
	if len(rs) == 0 {
		return nil, false, nil
	}
	return rs[0], true, nil
}

func (e *dbDirectory) txChangeParent(ctx context.Context, exec database.IExecer, entryid uint64, parentid uint64, newname *string) error {
	where := map[string]interface{}{
		"entry_id": entryid,
	}
	update := map[string]interface{}{
		"parent_entry_id": parentid,
	}
	if newname != nil {
		update["file_name"] = *newname
	}
	sql, args, err := builder.BuildUpdate(e.table(), where, update)
	if err != nil {
		return err
	}
	rs, err := exec.ExecContext(ctx, sql, args...)
	if err != nil {
		return err
	}
	cnt, err := rs.RowsAffected()
	if err != nil {
		return err
	}
	if cnt == 0 {
		return fmt.Errorf("no row affected")
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

func (e *dbDirectory) txCreateEntry(ctx context.Context, exec database.IExecer, pid uint64, ent *directoryEntryTab) (uint64, error) {
	eid := e.newEntryId()
	data := []map[string]interface{}{
		{
			"entry_id":        eid,
			"parent_entry_id": pid,
			"ref_data":        ent.RefData,
			"file_kind":       ent.FileKind,
			"ctime":           ent.Ctime,
			"mtime":           ent.Mtime,
			"file_size":       ent.FileSize,
			"file_mode":       ent.FileMode,
			"file_name":       ent.FileName,
		},
	}
	sql, args, err := builder.BuildInsert(e.table(), data)
	if err != nil {
		return 0, err
	}
	rs, err := exec.ExecContext(ctx, sql, args...)
	if err != nil {
		return 0, err
	}
	cnt, err := rs.RowsAffected()
	if err != nil {
		return 0, err
	}
	if cnt == 0 {
		return 0, fmt.Errorf("insert record failed, no row inserted")
	}
	return eid, nil
}

func (e *dbDirectory) txRemove(ctx context.Context, tx database.IExecer, parentid uint64, name string) error {
	where := map[string]interface{}{
		"parent_entry_id": parentid,
		"file_name":       name,
	}
	sql, args, err := builder.BuildDelete(e.table(), where)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, sql, args...); err != nil {
		return err
	}
	return nil
}

func (e *dbDirectory) txCreateDir(ctx context.Context, exec database.IExecer, pid uint64, name string) (uint64, error) {
	now := time.Now().UnixMilli()
	ent := &directoryEntryTab{
		ParentEntryId: pid,
		RefData:       "",
		FileKind:      defaultFileKindDir,
		Ctime:         now,
		Mtime:         now,
		FileSize:      0,
		FileMode:      defaultEntryFileMode,
		FileName:      name,
	}
	return e.txCreateEntry(ctx, exec, pid, ent)
}

func (e *dbDirectory) txCreateFile(ctx context.Context, exec database.IExecer, pid uint64, ent *directoryEntryTab) (uint64, error) {
	return e.txCreateEntry(ctx, exec, pid, ent)
}

func (e *dbDirectory) txListDir(ctx context.Context, q database.IQueryer, parentid uint64, offset, limit int64) ([]*directoryEntryTab, error) {
	where := map[string]interface{}{
		"parent_entry_id": parentid,
		"_limit":          []uint{uint(offset), uint(limit)},
	}
	rs := make([]*directoryEntryTab, 0, limit)
	if err := dbkit.SimpleQuery(ctx, q, e.table(), where, &rs, dbkit.ScanWithTagName("json")); err != nil {
		return nil, err
	}
	return rs, nil
}

func (e *dbDirectory) txListAllDir(ctx context.Context, q database.IQueryExecer, parentid uint64) ([]*directoryEntryTab, error) {
	var offset int64
	const limit int64 = 128
	rs := make([]*directoryEntryTab, 0, limit)
	for offset = 0; ; offset += limit {
		ents, err := e.txListDir(ctx, q, parentid, offset, limit)
		if err != nil {
			return nil, err
		}
		rs = append(rs, ents...)
		if int64(len(ents)) < limit {
			break
		}
	}
	return rs, nil
}

func (e *dbDirectory) txOnSelectDir(ctx context.Context, tx database.IQueryExecer, dir string, allowCreate bool, cb onSelectDirFunc) error {
	//逐级查找并创建目录, 返回最后的目录的id
	items, err := e.rebuildDirItems(dir)
	if err != nil {
		return err
	}
	if len(items) > defaultMaxDepthLimit {
		return fmt.Errorf("depth out of limit, current:%d", len(items))
	}
	var parentid uint64
	for idx, item := range items {
		ent, ok, err := e.txSearchEntry(ctx, tx, parentid, item)
		if err != nil {
			return err
		}
		if !ok {
			if !allowCreate {
				return fmt.Errorf("dir not found")
			}
			pid, err := e.txCreateDir(ctx, tx, parentid, item)
			if err != nil {
				return err
			}
			parentid = pid
			continue
		}
		if ent.FileKind != defaultFileKindDir {
			return fmt.Errorf("found non-dir in sub path, sub:%s", strings.Join(items[:idx+1], "/"))
		}
		parentid = ent.EntryId
	}
	return cb(ctx, parentid, tx)
}

func (e *dbDirectory) txGetRoot(ctx context.Context, tx database.IQueryExecer) (*directoryEntryTab, bool, error) {
	return e.txSearchEntry(ctx, tx, 0, "/")
}

func (e *dbDirectory) txCreateRoot(ctx context.Context, tx database.IQueryExecer) (*directoryEntryTab, error) {
	ent, ok, err := e.txGetRoot(ctx, tx)
	if err != nil {
		return nil, err
	}
	if ok {
		return ent, nil
	}
	now := time.Now().UnixMilli()
	_, err = e.txCreateEntry(ctx, tx, 0, &directoryEntryTab{
		ParentEntryId: 0,
		RefData:       "",
		FileKind:      defaultFileKindDir,
		Ctime:         now,
		Mtime:         now,
		FileSize:      0,
		FileMode:      defaultEntryFileMode,
		FileName:      "/",
	})
	if err != nil {
		return nil, err
	}
	ent, ok, err = e.txGetRoot(ctx, tx)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("create root but still not found")
	}
	return ent, nil
}

func (e *dbDirectory) txGetEntryInfo(ctx context.Context, tx database.IQueryExecer, location string, exportLastParentId *uint64) (*directoryEntryTab, bool, error) {
	dir, name, isRoot := e.splitFilename(location)
	if isRoot {
		return e.txGetRoot(ctx, tx)
	}
	var sinfo *directoryEntryTab
	var exist bool
	if err := e.txOnSelectDir(ctx, tx, dir, false, func(ctx context.Context, parentid uint64, tx database.IQueryExecer) error {
		if exportLastParentId != nil {
			*exportLastParentId = parentid
		}
		ent, ok, err := e.txSearchEntry(ctx, tx, parentid, name)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		sinfo = ent
		exist = true
		return nil
	}); err != nil {
		return nil, false, err
	}
	if !exist {
		return nil, false, nil
	}
	return sinfo, true, nil
}

func (e *dbDirectory) onSelectDir(ctx context.Context, dir string, allowCreate bool, cb onSelectDirFunc) error {
	if err := e.db.OnTransation(ctx, func(ctx context.Context, qe database.IQueryExecer) error {
		return e.txOnSelectDir(ctx, qe, dir, allowCreate, cb)
	}); err != nil {
		return err
	}
	return nil
}

func (e *dbDirectory) Mkdir(ctx context.Context, dir string) error {
	pdir, name, isRoot := e.splitFilename(dir)
	if isRoot {
		if _, err := e.txCreateRoot(ctx, e.db); err != nil {
			return err
		}
		return nil
	}

	if err := e.onSelectDir(ctx, pdir, true, func(ctx context.Context, parentid uint64, tx database.IQueryExecer) error {
		exist, err := e.txIsEntryExist(ctx, tx, parentid, name)
		if err != nil {
			return err
		}
		if exist {
			return nil
		}
		if _, err := e.txCreateDir(ctx, tx, parentid, name); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return err
	}
	return nil
}

func (e *dbDirectory) doTxIterAndCopy(ctx context.Context, tx database.IQueryExecer, srcinfo *directoryEntryTab, dstparent uint64) error {
	now := time.Now().UnixMilli()
	dstentid, err := e.txCreateEntry(ctx, tx, dstparent, &directoryEntryTab{
		ParentEntryId: dstparent,
		RefData:       srcinfo.RefData,
		FileKind:      srcinfo.FileKind,
		Ctime:         now,
		Mtime:         now,
		FileSize:      srcinfo.FileSize,
		FileMode:      srcinfo.FileMode,
		FileName:      srcinfo.FileName,
	})
	if err != nil {
		return err
	}
	if srcinfo.FileKind == defaultFileKindFile { //如果是文件, 则直接结束
		return nil
	}
	items, err := e.txListAllDir(ctx, tx, srcinfo.EntryId)
	if err != nil {
		return fmt.Errorf("list all dir failed, eid:%d, err:%w", srcinfo.EntryId, err)
	}
	for _, item := range items { //递归创建子节点
		if err := e.doTxIterAndCopy(ctx, tx, item, dstentid); err != nil {
			return err
		}
	}
	return nil
}

func (e *dbDirectory) doTxCopy(ctx context.Context, tx database.IQueryExecer, src, dst string, overwrite bool) error {
	next, err := e.precheckMoveCopy(src, dst)
	if err != nil {
		return fmt.Errorf("precheck copy failed, err:%w", err)
	}
	if !next {
		return nil
	}

	sinfo, exist, err := e.txGetEntryInfo(ctx, tx, src, nil)
	if err != nil {
		return err
	}
	if !exist {
		return fmt.Errorf("src not found, loc:%s", src)
	}
	var dstparentid uint64
	dinfo, exist, err := e.txGetEntryInfo(ctx, tx, dst, &dstparentid)
	if err != nil {
		return err
	}
	if exist {
		if dinfo.FileKind == defaultFileKindDir { //存在且目标为目录, 直接跳过后续流程
			return fmt.Errorf("copy dst dir exist, skip next")
		}
		//如果为文件, 则需要检查是否启用overwrite
		if !overwrite {
			return fmt.Errorf("dst exist and overwrite = false, skip next")
		}
		if err := e.txRemove(ctx, tx, dinfo.ParentEntryId, dinfo.FileName); err != nil {
			return fmt.Errorf("delete before copy failed, err:%w", err)
		}
	}
	//执行递归copy流程
	if err := e.doTxIterAndCopy(ctx, tx, sinfo, dstparentid); err != nil {
		return fmt.Errorf("do iter copy failed, srcparentid:%d, dstparentid:%d, sname:%s, err:%w", sinfo.ParentEntryId, dstparentid, sinfo.FileName, err)
	}
	return nil
}

func (e *dbDirectory) Copy(ctx context.Context, src string, dst string, overwrite bool) error {
	if err := e.db.OnTransation(ctx, func(ctx context.Context, tx database.IQueryExecer) error {
		return e.doTxCopy(ctx, tx, src, dst, overwrite)
	}); err != nil {
		return fmt.Errorf("do copy failed, err:%w", err)
	}
	return nil
}

func (e *dbDirectory) precheckMoveCopy(src string, dst string) (bool, error) {
	s, err := e.rebuildDirItems(src)
	if err != nil {
		return false, err
	}
	d, err := e.rebuildDirItems(dst)
	if err != nil {
		return false, err
	}
	if e.isArrayEqual(s, d) {
		return false, nil
	}
	if e.isArrayHasSuffix(d, s) {
		return false, fmt.Errorf("dst is sub dir of src")
	}
	return true, nil
}

func (e *dbDirectory) doTxMove(ctx context.Context, tx database.IQueryExecer, src, dst string, overwrite bool) error {
	//将/a/b/1.txt 移动到目录/c下, 那么src = /a/b/1.txt, dst = /c/1.txt
	next, err := e.precheckMoveCopy(src, dst)
	if err != nil {
		return fmt.Errorf("pre check move failed, err:%w", err)
	}
	if !next {
		return nil
	}
	sinfo, ok, err := e.txGetEntryInfo(ctx, tx, src, nil)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("move src not found, location:%s", src)
	}
	//处理move流程
	var parentid uint64
	dinfo, dexist, err := e.txGetEntryInfo(ctx, tx, dst, &parentid)
	if err != nil {
		return err
	}
	_, dname, isRoot := e.splitFilename(dst)
	if isRoot {
		return fmt.Errorf("entry should mount to root, dst name should not be root")
	}
	if !dexist { //目标不存在, 那么直接把src挂到dst的parent上即可
		return e.txChangeParent(ctx, tx, sinfo.EntryId, parentid, &dname)
	}
	if dinfo.FileKind == defaultFileKindDir { //不允许直接覆盖dir
		return fmt.Errorf("not allow to overwrite dir")
	}
	if !overwrite { //文件存在, 但是又没又overwrite选项, 直接返回
		return fmt.Errorf("overwrite = false and file exist")
	}
	//删除老的, 并修改src父节点
	if err := e.txRemove(ctx, tx, parentid, dname); err != nil {
		return fmt.Errorf("overwrite but remove origin failed, err:%w", err)
	}
	return e.txChangeParent(ctx, tx, sinfo.EntryId, parentid, &dname)
}

func (e *dbDirectory) Move(ctx context.Context, src string, dst string, overwrite bool) error {
	if err := e.db.OnTransation(ctx, func(ctx context.Context, tx database.IQueryExecer) error {
		return e.doTxMove(ctx, tx, src, dst, overwrite)
	}); err != nil {
		return fmt.Errorf("do move failed, err:%w", err)
	}
	return nil

}

func (e *dbDirectory) txDoRemove(ctx context.Context, tx database.IQueryExecer, parentid uint64, name string) error {
	ent, ok, err := e.txSearchEntry(ctx, tx, parentid, name)
	if err != nil {
		return fmt.Errorf("read entry failed, pid:%d, name:%s", parentid, name)
	}
	if !ok { //已经被删了
		return nil
	}
	if ent.FileKind == defaultFileKindDir {
		items, err := e.txListAllDir(ctx, tx, ent.EntryId)
		if err != nil {
			return fmt.Errorf("scan entry from pid:%d failed, err:%w", parentid, err)
		}
		for _, item := range items {
			if err := e.txDoRemove(ctx, tx, item.ParentEntryId, item.FileName); err != nil {
				return err
			}
		}
	}
	return e.txRemove(ctx, tx, parentid, name)
}

func (e *dbDirectory) Remove(ctx context.Context, filename string) error {
	//递归删除其子节点, 再删除父节点
	dir, name, isRoot := e.splitFilename(filename)
	if err := e.db.OnTransation(ctx, func(ctx context.Context, tx database.IQueryExecer) error {
		if isRoot {
			return e.txDoRemove(ctx, tx, 0, "/")
		}
		return e.txOnSelectDir(ctx, tx, dir, false, func(ctx context.Context, parentid uint64, tx database.IQueryExecer) error {
			return e.txDoRemove(ctx, tx, parentid, name)
		})
	}); err != nil {
		return err
	}
	return nil
}

func (e *dbDirectory) Create(ctx context.Context, filename string, size int64, refdata string) error {
	filename = strings.TrimSuffix(filename, "/")
	dir, name, isRoot := e.splitFilename(filename)
	if isRoot {
		return fmt.Errorf("root node should be dir")
	}
	if err := e.onSelectDir(ctx, dir, true, func(ctx context.Context, parentid uint64, tx database.IQueryExecer) error {
		exist, err := e.txIsEntryExist(ctx, tx, parentid, name)
		if err != nil {
			return err
		}
		if exist {
			return fmt.Errorf("file exist, skip create")
		}
		now := time.Now().UnixMilli()
		if _, err := e.txCreateFile(ctx, tx, parentid, &directoryEntryTab{
			RefData:  refdata,
			FileKind: 2,
			Ctime:    now,
			Mtime:    now,
			FileSize: size,
			FileMode: defaultEntryFileMode,
			FileName: name,
		}); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return err
	}
	return nil
}

func (e *dbDirectory) List(ctx context.Context, dir string) ([]*DirectoryEntry, error) {
	var rs []*DirectoryEntry
	if err := e.onSelectDir(ctx, dir, false, func(ctx context.Context, parentid uint64, tx database.IQueryExecer) error {
		items, err := e.txListAllDir(ctx, tx, parentid)
		if err != nil {
			return err
		}
		for _, item := range items {
			rs = append(rs, item.ToDirectoyEntry())
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return rs, nil
}

func (e *dbDirectory) Stat(ctx context.Context, filename string) (*DirectoryEntry, error) {
	dir, name, isRoot := e.splitFilename(filename)
	if isRoot {
		ent, ok, err := e.txGetRoot(ctx, e.db)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("root node not found")
		}
		return ent.ToDirectoyEntry(), nil
	}
	var rs *DirectoryEntry
	if err := e.onSelectDir(ctx, dir, true, func(ctx context.Context, parentid uint64, tx database.IQueryExecer) error {
		t, ok, err := e.txSearchEntry(ctx, tx, parentid, name)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("file not found, path:%s", filename)
		}
		rs = t.ToDirectoyEntry()
		return nil
	}); err != nil {
		return nil, err
	}
	return rs, nil
}

func NewDBDirectory(db database.IDatabase, tab string, idfn IDGenFunc) (IDirectory, error) {
	return &dbDirectory{
		db:   db,
		tab:  tab,
		idfn: idfn,
	}, nil
}
