package directory

import (
	"context"
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	"github.com/didi/gendry/builder"
	"github.com/xxxsen/common/database"
	"github.com/xxxsen/common/database/dbkit"
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
	var parentid uint64
	for idx, item := range items {
		ent, ok, err := e.txSearchEntry(ctx, tx, parentid, item)
		if err != nil {
			return err
		}
		if !ok {
			if !allowCreate {
				return os.ErrNotExist
			}
			pid, err := e.txCreateDir(ctx, tx, parentid, item)
			if err != nil {
				return err
			}
			parentid = pid
			continue
		}
		if ent.FileKind_ != defaultFileKindDir {
			return fmt.Errorf("found non-dir in sub path, sub:%s", strings.Join(items[:idx+1], "/"))
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
		return nil, err
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

func (e *dbDirectory) txDoIterAndCopy(ctx context.Context, tx database.IQueryExecer, srcinfo *directoryEntryTab, dstparent uint64, newname string) error {
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
		return err
	}
	if srcinfo.FileKind_ == defaultFileKindFile { //如果是文件, 则直接结束
		return nil
	}
	items, err := e.txListAllDir(ctx, tx, srcinfo.EntryId_)
	if err != nil {
		return fmt.Errorf("list all dir failed, eid:%d, err:%w", srcinfo.EntryId_, err)
	}
	for _, item := range items { //递归创建子节点
		if err := e.txDoIterAndCopy(ctx, tx, item, dstentid, item.FileName_); err != nil {
			return err
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
		return err
	}
	if !exist {
		return fmt.Errorf("src not found, loc:%s", src)
	}
	var dstparentid uint64
	_, dname, isRoot := e.splitFilename(dst)
	if isRoot {
		return fmt.Errorf("target should not be root, dst:%s", dst)
	}
	dinfo, exist, err := e.txGetEntryInfo(ctx, tx, dst, &dstparentid)
	if err != nil {
		return err
	}
	if exist {
		if dinfo.FileKind_ == defaultFileKindDir { //存在且目标为目录, 直接跳过后续流程
			return fmt.Errorf("copy dst dir exist, skip next")
		}
		//如果为文件, 则需要检查是否启用overwrite
		if !overwrite {
			return fmt.Errorf("dst exist and overwrite = false, skip next")
		}
		if err := e.txRemove(ctx, tx, dinfo.ParentEntryId_, dinfo.FileName_); err != nil {
			return fmt.Errorf("delete before copy failed, err:%w", err)
		}
	}
	//执行递归copy流程
	if err := e.txDoIterAndCopy(ctx, tx, sinfo, dstparentid, dname); err != nil {
		return fmt.Errorf("do iter copy failed, srcparentid:%d, dstparentid:%d, sname:%s, err:%w", sinfo.ParentEntryId_, dstparentid, sinfo.FileName_, err)
	}
	return nil
}

func (e *dbDirectory) Copy(ctx context.Context, src string, dst string, overwrite bool) error {
	if err := e.db.OnTransation(ctx, func(ctx context.Context, tx database.IQueryExecer) error {
		return e.txDoCopy(ctx, tx, src, dst, overwrite)
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

func (e *dbDirectory) txDoMove(ctx context.Context, tx database.IQueryExecer, src, dst string, overwrite bool) error {
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
		return e.txChangeParent(ctx, tx, sinfo.EntryId_, parentid, &dname)
	}
	if dinfo.FileKind_ == defaultFileKindDir { //不允许直接覆盖dir
		return fmt.Errorf("not allow to overwrite dir")
	}
	if !overwrite { //文件存在, 但是又没又overwrite选项, 直接返回
		return fmt.Errorf("overwrite = false and file exist")
	}
	//删除老的, 并修改src父节点
	if err := e.txRemove(ctx, tx, parentid, dname); err != nil {
		return fmt.Errorf("overwrite but remove origin failed, err:%w", err)
	}
	return e.txChangeParent(ctx, tx, sinfo.EntryId_, parentid, &dname)
}

func (e *dbDirectory) Move(ctx context.Context, src string, dst string, overwrite bool) error {
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
		return fmt.Errorf("read entry failed, pid:%d, name:%s", parentid, name)
	}
	if !ok { //已经被删了
		return nil
	}
	if ent.FileKind_ == defaultFileKindDir {
		items, err := e.txListAllDir(ctx, tx, ent.EntryId_)
		if err != nil {
			return fmt.Errorf("scan entry from pid:%d failed, err:%w", parentid, err)
		}
		for _, item := range items {
			if err := e.txDoRemove(ctx, tx, item.ParentEntryId_, item.FileName_); err != nil {
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
			return err
		}
		return nil
	}); err != nil {
		return err
	}
	return nil
}

func (e *dbDirectory) List(ctx context.Context, dir string) ([]IDirectoryEntry, error) {
	rs := make([]IDirectoryEntry, 0, 16)
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

func (e *dbDirectory) Stat(ctx context.Context, filename string) (IDirectoryEntry, error) {
	dir, name, isRoot := e.splitFilename(filename)
	if isRoot {
		ent, ok, err := e.txGetRoot(ctx, e.db)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, os.ErrNotExist
		}
		return ent.ToDirectoyEntry(), nil
	}
	var rs IDirectoryEntry
	if err := e.onSelectDir(ctx, dir, true, func(ctx context.Context, parentid uint64, tx database.IQueryExecer) error {
		t, ok, err := e.txSearchEntry(ctx, tx, parentid, name)
		if err != nil {
			return err
		}
		if !ok {
			return os.ErrNotExist
		}
		rs = t.ToDirectoyEntry()
		return nil
	}); err != nil {
		return nil, err
	}
	return rs, nil
}

func (e *dbDirectory) Scan(ctx context.Context, batch int64, cb DirectoryScanCallbackFunc) error {
	var lastid uint64
	for {
		res, nextid, err := e.innerScan(ctx, lastid, batch)
		if err != nil {
			return err
		}
		lastid = nextid
		next, err := cb(ctx, res)
		if err != nil {
			return err
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

func (e *dbDirectory) innerScan(ctx context.Context, lastid uint64, batch int64) ([]IDirectoryEntry, uint64, error) {
	where := map[string]interface{}{
		"id >":     lastid,
		"_orderby": "id asc",
		"_limit":   []uint{0, uint(batch)},
	}
	rs := make([]*directoryEntryTab, 0, batch)
	if err := dbkit.SimpleQuery(ctx, e.db, e.table(), where, &rs, dbkit.ScanWithTagName("json")); err != nil {
		return nil, 0, err
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
