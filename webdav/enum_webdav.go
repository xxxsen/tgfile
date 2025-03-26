package webdav

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/didi/gendry/builder"
	"github.com/xxxsen/common/database"
	"github.com/xxxsen/common/database/dbkit"
	"github.com/xxxsen/common/idgen"
)

const (
	defaultWebDavSql = `
CREATE TABLE IF NOT EXISTS %s (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    entry_id      INTEGER NOT NULL,
    parent_entry_id INTEGER NOT NULL,
    ref_data      TEXT,
    file_kind     INTEGER,
    ctime         INTEGER,
    mtime         INTEGER,
    file_size     INTEGER,
    file_mode     INTEGER,
    file_name     TEXT NOT NULL,
    UNIQUE (parent_entry_id, file_name)
);
	`
)

const (
	defaultMaxDepthLimit = 16
)

type onSelectDirFunc func(ctx context.Context, parentid uint64, tx database.IQueryExecer) error

type enumWebdav struct {
	db  database.IDatabase
	tab string
	gen idgen.IDGenerator
}

func (e *enumWebdav) rebuildDirItems(dir string) ([]string, error) {
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

func (e *enumWebdav) table() string {
	return e.tab
}

func (e *enumWebdav) splitFilename(filename string) (string, string) {
	filename = filepath.Clean(filename)
	if filename == "/" {
		return "", "/"
	}
	filename = strings.TrimSuffix(filename, "/")
	dir := filepath.Dir(filename)
	name := filepath.Base(filename)
	return dir, name
}

func (e *enumWebdav) searchEntry(ctx context.Context, q database.IQueryer, pid uint64, name string) (*webdavEntryTab, bool, error) {
	where := map[string]interface{}{
		"parent_entry_id": pid,
		"file_name":       name,
		"_limit":          []uint{0, 1},
	}
	rs := make([]*webdavEntryTab, 0, 1)
	if err := dbkit.SimpleQuery(ctx, q, e.table(), where, &rs, dbkit.ScanWithTagName("json")); err != nil {
		return nil, false, err
	}
	if len(rs) == 0 {
		return nil, false, nil
	}
	return rs[0], true, nil
}

func (e *enumWebdav) isEntryExist(ctx context.Context, q database.IQueryer, pid uint64, name string) (bool, error) {
	_, ok, err := e.searchEntry(ctx, q, pid, name)
	if err != nil {
		return false, err
	}
	return ok, nil
}

func (e *enumWebdav) newEntryId() uint64 {
	return e.gen.NextId()
}

func (e *enumWebdav) createEntry(ctx context.Context, exec database.IExecer, pid uint64, ent *webdavEntryTab) (uint64, error) {
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

func (e *enumWebdav) createDir(ctx context.Context, exec database.IExecer, pid uint64, name string) (uint64, error) {
	now := time.Now().UnixMilli()
	ent := &webdavEntryTab{
		ParentEntryId: pid,
		RefData:       "",
		FileKind:      defaultFileKindDir,
		Ctime:         now,
		Mtime:         now,
		FileSize:      0,
		FileMode:      defaultWebdavFileMode,
		FileName:      name,
	}
	return e.createEntry(ctx, exec, pid, ent)
}

func (e *enumWebdav) createFile(ctx context.Context, exec database.IExecer, pid uint64, ent *webdavEntryTab) (uint64, error) {
	return e.createEntry(ctx, exec, pid, ent)
}

func (e *enumWebdav) listDir(ctx context.Context, q database.IQueryer, parentid uint64, offset, limit int64) ([]*webdavEntryTab, error) {
	where := map[string]interface{}{
		"parent_entry_id": parentid,
		"_limit":          []uint{uint(offset), uint(limit)},
	}
	rs := make([]*webdavEntryTab, 0, limit)
	if err := dbkit.SimpleQuery(ctx, q, e.table(), where, &rs, dbkit.ScanWithTagName("json")); err != nil {
		return nil, err
	}
	return rs, nil
}

func (e *enumWebdav) listAllDir(ctx context.Context, q database.IQueryExecer, parentid uint64) ([]*webdavEntryTab, error) {
	var offset int64
	const limit int64 = 128
	rs := make([]*webdavEntryTab, 0, limit)
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

func (e *enumWebdav) onSelectDir(ctx context.Context, dir string, cb onSelectDirFunc) error {
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
		if len(items) == 1 && items[0] == "/" {
			return cb(ctx, parentid, qe)
		}

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

func (e *enumWebdav) Mkdir(ctx context.Context, dir string) error {
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

func (e *enumWebdav) Copy(ctx context.Context, src string, dst string, overwrite bool) error {
	return fmt.Errorf("not impl yet")
}

func (e *enumWebdav) Move(ctx context.Context, src string, dst string, overwrite bool) error {
	return fmt.Errorf("not impl yet")
}

func (e *enumWebdav) Remove(ctx context.Context, filename string) error {
	//TODO: 删除其子节点, 再删除父节点
	return fmt.Errorf("not impl yet")
}

func (e *enumWebdav) Create(ctx context.Context, filename string, size int64, refdata string) error {
	if strings.HasSuffix(filename, "/") {
		return fmt.Errorf("filename should not endswith '/'")
	}
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
		if _, err := e.createFile(ctx, tx, parentid, &webdavEntryTab{
			RefData:  refdata,
			FileKind: 2,
			Ctime:    now,
			Mtime:    now,
			FileSize: size,
			FileMode: defaultWebdavFileMode,
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

func (e *enumWebdav) List(ctx context.Context, dir string) ([]*WebEntry, error) {
	var rs []*WebEntry
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

func (e *enumWebdav) Stat(ctx context.Context, filename string) (*WebEntry, error) {
	dir, name := e.splitFilename(filename)
	var rs *WebEntry
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

func (e *enumWebdav) convTabDataToEntryData(item *webdavEntryTab) *WebEntry {
	return &WebEntry{
		RefData: item.RefData,
		Name:    item.FileName,
		Ctime:   item.Ctime,
		Mtime:   item.Mtime,
		Mode:    item.FileMode,
		Size:    item.FileSize,
		IsDir:   item.FileKind == defaultFileKindDir,
	}
}

func NewEnumWebdav(db database.IDatabase, tab string) (IWebdav, error) {
	gen := idgen.Default()
	if _, err := db.ExecContext(context.Background(), fmt.Sprintf(defaultWebDavSql, tab)); err != nil {
		return nil, fmt.Errorf("init table structure failed, err:%w", err)
	}

	return &enumWebdav{
		db:  db,
		tab: tab,
		gen: gen,
	}, nil
}
