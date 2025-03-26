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

/*
id int64
fname string
fsize int64
parent_id int64
ctime int64
mtime int64
mode int64

*/

const (
	defaultMaxDepthLimit = 16
)

type WebdavEntryTab struct {
	Id            uint64 `json:"id"`
	EntryId       uint64 `json:"entry_id"`
	ParentEntryId uint64 `json:"parent_entry_id"`
	RefData       string `json:"ref_data"`
	FileKind      int32  `json:"file_kind"`
	Ctime         int64  `json:"ctime"`
	Mtime         int64  `json:"mtime"`
	FileSize      int64  `json:"file_size"`
	FileMode      uint32 `json:"file_mode"`
	FileName      string `json:"file_name"`
}

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

func (e *enumWebdav) searchEntry(ctx context.Context, q database.IQueryer, pid uint64, name string) (*WebdavEntryTab, bool, error) {
	where := map[string]interface{}{
		"parent_entry_id": pid,
		"name":            name,
		"_limit":          []uint{0, 1},
	}
	rs := make([]*WebdavEntryTab, 0, 1)
	if err := dbkit.SimpleQuery(ctx, q, e.tab, where, &rs, dbkit.ScanWithTagName("json")); err != nil {
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

func (e *enumWebdav) createEntry(ctx context.Context, exec database.IExecer, pid uint64, ent *WebdavEntryTab) (uint64, error) {
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
	sql, args, err := builder.BuildInsert(e.tab, data)
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
	ent := &WebdavEntryTab{
		ParentEntryId: pid,
		RefData:       "{}",
		FileKind:      defaultFileKindDir,
		Ctime:         now,
		Mtime:         now,
		FileSize:      0,
		FileMode:      defaultDirMode,
		FileName:      name,
	}
	return e.createEntry(ctx, exec, pid, ent)
}

func (e *enumWebdav) createFile(ctx context.Context, exec database.IExecer, pid uint64, ent *WebdavEntryTab) (uint64, error) {
	return e.createEntry(ctx, exec, pid, ent)
}

func (e *enumWebdav) listDir(ctx context.Context, q database.IQueryer, parentid uint64, offset, limit int64) ([]*WebdavEntryTab, error) {
	where := map[string]interface{}{
		"parent_entry_id": parentid,
		"_limit":          []uint{uint(offset), uint(limit)},
	}
	rs := make([]*WebdavEntryTab, 0, limit)
	if err := dbkit.SimpleQuery(ctx, q, e.tab, where, &rs, dbkit.ScanWithTagName("json")); err != nil {
		return nil, err
	}
	return rs, nil
}

func (e *enumWebdav) listAllDir(ctx context.Context, q database.IQueryExecer, parentid uint64) ([]*WebdavEntryTab, error) {
	var offset int64
	const limit int64 = 128
	rs := make([]*WebdavEntryTab, 0, limit)
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
			parentid = ent.ParentEntryId
		}
		return cb(ctx, parentid, qe)
	}); err != nil {
		return err
	}
	return nil
}

func (e *enumWebdav) Mkdir(ctx context.Context, dir string, mode uint32) error {
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
	panic("TODO: Implement")
}

func (e *enumWebdav) Move(ctx context.Context, src string, dst string, overwrite bool) error {
	panic("TODO: Implement")
}

func (e *enumWebdav) Create(ctx context.Context, link string, ent *WebEntry) error {
	if strings.HasSuffix(link, "/") {
		return fmt.Errorf("filename should not endswith '/'")
	}
	dir := filepath.Dir(link)
	name := filepath.Base(link)
	ent.Name = name
	if err := e.onSelectDir(ctx, dir, func(ctx context.Context, parentid uint64, tx database.IQueryExecer) error {
		exist, err := e.isEntryExist(ctx, tx, parentid, name)
		if err != nil {
			return err
		}
		if exist {
			return fmt.Errorf("file exist, skip create")
		}
		if _, err := e.createFile(ctx, tx, parentid, &WebdavEntryTab{
			RefData:  ent.RefData,
			FileKind: 2,
			Ctime:    ent.Ctime,
			Mtime:    ent.Mtime,
			FileSize: ent.Size,
			FileMode: uint32(ent.Mode),
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
	dir := filepath.Dir(filename)
	name := filepath.Base(filename)
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

func (e *enumWebdav) convTabDataToEntryData(item *WebdavEntryTab) *WebEntry {
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
	return &enumWebdav{
		db:  db,
		tab: tab,
		gen: gen,
	}, nil
}
