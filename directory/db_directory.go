package directory

import (
	"context"
	"fmt"
	"path/filepath"
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

func (e *dbDirectory) splitFilename(filename string) (string, string) {
	filename = filepath.Clean(filename)
	if filename == "/" {
		return "", "/"
	}
	filename = strings.TrimSuffix(filename, "/")
	dir := filepath.Dir(filename)
	name := filepath.Base(filename)
	return dir, name
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

func (e *dbDirectory) onSelectDir(ctx context.Context, dir string, allowCreate bool, cb onSelectDirFunc) error {
	if err := e.db.OnTransation(ctx, func(ctx context.Context, qe database.IQueryExecer) error {
		return e.txOnSelectDir(ctx, qe, dir, allowCreate, cb)
	}); err != nil {
		return err
	}
	return nil
}

func (e *dbDirectory) Mkdir(ctx context.Context, dir string) error {
	pdir, name := e.splitFilename(dir)
	if err := e.onSelectDir(ctx, pdir, true, func(ctx context.Context, parentid uint64, tx database.IQueryExecer) error {
		exist, err := e.txIsEntryExist(ctx, tx, parentid, name)
		if err != nil {
			return err
		}
		if exist {
			return fmt.Errorf("dir exist, skip create")
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

func (e *dbDirectory) Copy(ctx context.Context, src string, dst string, overwrite bool) error {
	return fmt.Errorf("not impl yet")
}

func (d *dbDirectory) doMoveOnExist(ctx context.Context, tx database.IQueryExecer,
	sinfo *directoryEntryTab, dparentid uint64, dname string, dinfo *directoryEntryTab, overwrite bool) error {
	if !overwrite {
		return fmt.Errorf("cant move to exist entry")
	}
	if dinfo.FileKind == defaultFileKindFile && sinfo.FileKind == defaultFileKindDir {
		return fmt.Errorf("cant move dir entry to file entry")
	}
	now := time.Now().UnixMilli()
	//目标文件为file, 则原地覆盖
	if dinfo.FileKind == defaultFileKindFile {
		if err := d.txRemove(ctx, tx, dinfo.ParentEntryId, dinfo.FileName); err != nil {
			return fmt.Errorf("remove dst file before move fialed, err:%w", err)
		}
		if _, err := d.txCreateEntry(ctx, tx, dinfo.ParentEntryId, &directoryEntryTab{
			ParentEntryId: dinfo.ParentEntryId,
			RefData:       sinfo.RefData,
			FileKind:      sinfo.FileKind,
			Ctime:         now,
			Mtime:         now,
			FileSize:      sinfo.FileSize,
			FileMode:      sinfo.FileMode,
			FileName:      dinfo.FileName,
		}); err != nil {
			return fmt.Errorf("create dst file failed, err:%w", err)
		}
		return nil
	}
	//目标文件为dir, 需要检查dir下是否还有文件名与sname相同的, 有的话, 需要先删除
	//如果dir下与sname同名的为目录, 则返回错误
	// 之后再在dir下面创建文件
	entryInDst, ok, err := d.txSearchEntry(ctx, tx, dinfo.EntryId, sinfo.FileName)
	if err != nil {
		return fmt.Errorf("check src name in dst dir failed, err:%w", err)
	}
	//不存在同名则直接创建
	if !ok {
		if _, err := d.txCreateEntry(ctx, tx, dinfo.EntryId, &directoryEntryTab{
			ParentEntryId: dinfo.EntryId,
			RefData:       sinfo.RefData,
			FileKind:      sinfo.FileKind,
			Ctime:         now,
			Mtime:         now,
			FileSize:      sinfo.FileSize,
			FileMode:      sinfo.FileMode,
			FileName:      sinfo.FileName,
		}); err != nil {
			return fmt.Errorf("create src file in dst dir failed, err:%w", err)
		}
		return nil
	}
	//在目标目录下有跟src同名, 又又又得检查一遍这个东西是文件还是目录
	if entryInDst.FileKind == defaultFileKindDir {
		return fmt.Errorf("same name dir entry found in dst dir, skip move")
	}
	//接下来就把对应的文件删掉, 然后重新创建新的即可
	if err := d.txRemove(ctx, tx, dinfo.EntryId, sinfo.FileName); err != nil {
		return fmt.Errorf("remove same name file in dst dir failed, err:%w", err)
	}
	if _, err := d.txCreateEntry(ctx, tx, dinfo.EntryId, &directoryEntryTab{
		ParentEntryId: dinfo.EntryId,
		RefData:       sinfo.RefData,
		FileKind:      sinfo.FileKind,
		Ctime:         now,
		Mtime:         now,
		FileSize:      sinfo.FileSize,
		FileMode:      sinfo.FileMode,
		FileName:      sinfo.FileName,
	}); err != nil {
		return fmt.Errorf("create entry in dst dir failed, err:%w", err)
	}
	return nil
}

func (d *dbDirectory) doMoveOnNonExist(ctx context.Context, tx database.IQueryExecer,
	sinfo *directoryEntryTab, dparentid uint64, dname string, dinfo *directoryEntryTab, overwrite bool) error {
	if _, err := d.txCreateEntry(ctx, tx, dparentid, &directoryEntryTab{
		ParentEntryId: dparentid,
		RefData:       sinfo.RefData,
		FileKind:      sinfo.FileKind,
		Ctime:         time.Now().UnixMilli(),
		Mtime:         time.Now().UnixMilli(),
		FileSize:      sinfo.FileSize,
		FileMode:      sinfo.FileMode,
		FileName:      dname,
	}); err != nil {
		return fmt.Errorf("create src file in non-exist dst failed, err:%w", err)
	}
	return nil
}

func (e *dbDirectory) Move(ctx context.Context, src string, dst string, overwrite bool) error {
	sdir, sname := e.splitFilename(src)
	ddir, dname := e.splitFilename(dst)
	//TODO: 这里检查下sdir/ddir是否一致
	if err := e.db.OnTransation(ctx, func(ctx context.Context, tx database.IQueryExecer) error {
		var sinfo *directoryEntryTab
		//提取原始信息
		if err := e.txOnSelectDir(ctx, tx, sdir, false, func(ctx context.Context, parentid uint64, tx database.IQueryExecer) error {
			info, ok, err := e.txSearchEntry(ctx, tx, parentid, sname)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("src file not found, err:%w", err)
			}
			sinfo = info
			return nil
		}); err != nil {
			return fmt.Errorf("read src info failed, err:%w", err)
		}
		//处理move流程
		if err := e.txOnSelectDir(ctx, tx, ddir, false, func(ctx context.Context, parentid uint64, tx database.IQueryExecer) error {
			dinfo, dexist, err := e.txSearchEntry(ctx, tx, parentid, dname)
			if err != nil {
				return err
			}
			//dst存在的情况下
			// - 如果为dst目录, src会被复制到dst下面
			// - 如果dst为文件, 那么src会直接覆盖dst，并重命名为dst
			//不存在的情况下
			// - 直接将src复制到dst下
			//无论哪种情况, 最终都要删除src
			handler := e.doMoveOnExist
			if !dexist {
				handler = e.doMoveOnNonExist
			}
			if err := handler(ctx, tx, sinfo, parentid, dname, dinfo, overwrite); err != nil {
				return fmt.Errorf("handle move failed, dexist:%t, err:%w", dexist, err)
			}
			if err := e.txRemove(ctx, tx, sinfo.ParentEntryId, sinfo.FileName); err != nil {
				return fmt.Errorf("tx remove old file failed, pid:%d, name:%s, err:%w",
					sinfo.ParentEntryId, sinfo.FileName, err)
			}
			return nil
		}); err != nil {
			return fmt.Errorf("read dst and move failed, err:%w", err)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("do move failed, err:%w", err)
	}
	return nil

}

func (e *dbDirectory) doRemoveFile(ctx context.Context, tx database.IQueryExecer, parentid uint64, name string) error {
	return e.txRemove(ctx, tx, parentid, name)
}

func (e *dbDirectory) doRemoveDir(ctx context.Context, tx database.IQueryExecer, parentid uint64, name string) error {
	items, err := e.txListAllDir(ctx, tx, parentid)
	if err != nil {
		return fmt.Errorf("scan entry from pid:%d failed, err:%w", parentid, err)
	}
	for _, item := range items {
		if item.FileKind == defaultFileKindDir {
			if err := e.doRemoveDir(ctx, tx, item.EntryId, item.FileName); err != nil {
				return fmt.Errorf("remove dir entry failed, entryid:%d, entryname:%s, err:%d", item.EntryId, item.FileName, err)
			}
			continue
		}
		if err := e.doRemoveFile(ctx, tx, parentid, item.FileName); err != nil {
			return fmt.Errorf("remove file entry failed, parentid:%d, entryname:%s, err:%w", parentid, item.FileName, err)
		}
	}
	//删除目录自身
	if err := e.doRemoveFile(ctx, tx, parentid, name); err != nil {
		return fmt.Errorf("remove dir self failed, parentid:%d, name:%s, err:%w", parentid, name, err)
	}
	return nil
}

func (e *dbDirectory) Remove(ctx context.Context, filename string) error {
	//递归删除其子节点, 再删除父节点
	dir, name := e.splitFilename(filename)
	if err := e.onSelectDir(ctx, dir, false, func(ctx context.Context, parentid uint64, tx database.IQueryExecer) error {
		ent, ok, err := e.txSearchEntry(ctx, tx, parentid, name)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		if ent.FileKind == defaultFileKindDir {
			return e.doRemoveDir(ctx, tx, parentid, name)
		}
		return e.doRemoveFile(ctx, tx, parentid, name)
	}); err != nil {
		return err
	}
	return nil
}

func (e *dbDirectory) Create(ctx context.Context, filename string, size int64, refdata string) error {
	filename = strings.TrimSuffix(filename, "/")
	dir, name := e.splitFilename(filename)
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
			rs = append(rs, e.convTabDataToEntryData(item))
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return rs, nil
}

func (e *dbDirectory) Stat(ctx context.Context, filename string) (*DirectoryEntry, error) {
	dir, name := e.splitFilename(filename)
	if name == "/" {
		return &DirectoryEntry{
			RefData: "",
			Name:    name,
			Ctime:   0,
			Mtime:   0,
			Mode:    defaultEntryFileMode,
			Size:    0,
			IsDir:   true,
		}, nil
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
		rs = e.convTabDataToEntryData(t)
		return nil
	}); err != nil {
		return nil, err
	}
	return rs, nil
}

func (e *dbDirectory) convTabDataToEntryData(item *directoryEntryTab) *DirectoryEntry {
	return &DirectoryEntry{
		RefData: item.RefData,
		Name:    item.FileName,
		Ctime:   item.Ctime,
		Mtime:   item.Mtime,
		Mode:    item.FileMode,
		Size:    item.FileSize,
		IsDir:   item.FileKind == defaultFileKindDir,
	}
}

func NewDBDirectory(db database.IDatabase, tab string, idfn IDGenFunc) (IDirectory, error) {
	return &dbDirectory{
		db:   db,
		tab:  tab,
		idfn: idfn,
	}, nil
}
