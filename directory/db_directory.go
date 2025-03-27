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

func (e *dbDirectory) searchEntry(ctx context.Context, q database.IQueryer, pid uint64, name string) (*directoryEntryTab, bool, error) {
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

func (e *dbDirectory) isEntryExist(ctx context.Context, q database.IQueryer, pid uint64, name string) (bool, error) {
	_, ok, err := e.searchEntry(ctx, q, pid, name)
	if err != nil {
		return false, err
	}
	return ok, nil
}

func (e *dbDirectory) newEntryId() uint64 {
	return e.idfn()
}

func (e *dbDirectory) createEntry(ctx context.Context, exec database.IExecer, pid uint64, ent *directoryEntryTab) (uint64, error) {
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

func (e *dbDirectory) createDir(ctx context.Context, exec database.IExecer, pid uint64, name string) (uint64, error) {
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
	return e.createEntry(ctx, exec, pid, ent)
}

func (e *dbDirectory) createFile(ctx context.Context, exec database.IExecer, pid uint64, ent *directoryEntryTab) (uint64, error) {
	return e.createEntry(ctx, exec, pid, ent)
}

func (e *dbDirectory) listDir(ctx context.Context, q database.IQueryer, parentid uint64, offset, limit int64) ([]*directoryEntryTab, error) {
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

func (e *dbDirectory) listAllDir(ctx context.Context, q database.IQueryExecer, parentid uint64) ([]*directoryEntryTab, error) {
	var offset int64
	const limit int64 = 128
	rs := make([]*directoryEntryTab, 0, limit)
	for offset = 0; ; offset += limit {
		ents, err := e.listDir(ctx, q, parentid, offset, limit)
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

func (e *dbDirectory) onSelectDir(ctx context.Context, dir string, cb onSelectDirFunc) error {
	//逐级查找并创建目录, 返回最后的目录的id
	items, err := e.rebuildDirItems(dir)
	if err != nil {
		return err
	}
	if len(items) > defaultMaxDepthLimit {
		return fmt.Errorf("depth out of limit, current:%d", len(items))
	}
	var parentid uint64
	if err := e.db.OnTransation(ctx, func(ctx context.Context, qe database.IQueryExecer) error {
		for idx, item := range items {
			ent, ok, err := e.searchEntry(ctx, qe, parentid, item)
			if err != nil {
				return err
			}
			if !ok {
				pid, err := e.createDir(ctx, qe, parentid, item)
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
		return cb(ctx, parentid, qe)
	}); err != nil {
		return err
	}
	return nil
}

func (e *dbDirectory) Mkdir(ctx context.Context, dir string) error {
	pdir := filepath.Dir(dir)
	name := filepath.Base(dir)
	if err := e.onSelectDir(ctx, pdir, func(ctx context.Context, parentid uint64, tx database.IQueryExecer) error {
		exist, err := e.isEntryExist(ctx, tx, parentid, name)
		if err != nil {
			return err
		}
		if exist {
			return fmt.Errorf("dir exist, skip create")
		}
		if _, err := e.createDir(ctx, tx, parentid, name); err != nil {
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

func (e *dbDirectory) Move(ctx context.Context, src string, dst string, overwrite bool) error {
	return fmt.Errorf("not impl yet")
}

func (e *dbDirectory) Remove(ctx context.Context, filename string) error {
	//TODO: 删除其子节点, 再删除父节点
	return fmt.Errorf("not impl yet")
}

func (e *dbDirectory) Create(ctx context.Context, filename string, size int64, refdata string) error {
	filename = strings.TrimSuffix(filename, "/")
	dir, name := e.splitFilename(filename)
	if err := e.onSelectDir(ctx, dir, func(ctx context.Context, parentid uint64, tx database.IQueryExecer) error {
		exist, err := e.isEntryExist(ctx, tx, parentid, name)
		if err != nil {
			return err
		}
		if exist {
			return fmt.Errorf("file exist, skip create")
		}
		now := time.Now().UnixMilli()
		if _, err := e.createFile(ctx, tx, parentid, &directoryEntryTab{
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
	if err := e.onSelectDir(ctx, dir, func(ctx context.Context, parentid uint64, tx database.IQueryExecer) error {
		items, err := e.listAllDir(ctx, tx, parentid)
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
	if err := e.onSelectDir(ctx, dir, func(ctx context.Context, parentid uint64, tx database.IQueryExecer) error {
		t, ok, err := e.searchEntry(ctx, tx, parentid, name)
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
