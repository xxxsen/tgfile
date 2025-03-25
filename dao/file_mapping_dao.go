package dao

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"tgfile/constant"
	"tgfile/db"
	"tgfile/entity"

	"github.com/xxxsen/common/database/kv"
)

const (
	defaultFileMappingPrefix = "tgfile:mapping:"
)

type IterFileMappingFunc func(ctx context.Context, name string, ent *entity.FileMappingItem) (bool, error)

type IFileMappingDao interface {
	GetFileMapping(ctx context.Context, req *entity.GetFileMappingRequest) (*entity.GetFileMappingResponse, bool, error)
	CreateFileMapping(ctx context.Context, req *entity.CreateFileMappingRequest) (*entity.CreateFileMappingResponse, error)
	IterFileMapping(ctx context.Context, prefix string, cb IterFileMappingFunc) error
}

type fileMappingDao struct {
}

func NewFileMappingDao() IFileMappingDao {
	return &fileMappingDao{}
}

func (f *fileMappingDao) table() string {
	return "tg_file_mapping_tab"
}

func (f *fileMappingDao) buildKey(name string) string {
	return defaultFileMappingPrefix + name
}

func (f *fileMappingDao) GetFileMapping(ctx context.Context, req *entity.GetFileMappingRequest) (*entity.GetFileMappingResponse, bool, error) {
	item, ok, err := kv.GetJsonObject[entity.FileMappingItem](ctx, db.GetClient(), f.table(), f.buildKey(req.FileName))
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return nil, false, nil
	}
	return &entity.GetFileMappingResponse{Item: item}, true, nil
}

func (f *fileMappingDao) resolveSubPaths(filename string) ([]string, error) {
	filename = filepath.Dir(filename) //尝试构建父目录的所有子目录
	subs := strings.Split(filename, "/")
	rs := make([]string, 0, len(subs)+1)
	rs = append(rs, "/")
	for _, sub := range subs {
		sub = strings.TrimSpace(sub)
		if len(sub) == 0 {
			continue
		}
		if sub == "." || sub == ".." {
			return nil, fmt.Errorf("invalid character in path, c:`%s`", sub)
		}
		rs = append(rs, rs[len(rs)-1]+sub+"/")
	}
	return rs, nil
}

func (f *fileMappingDao) ensureTargetParent(ctx context.Context, tx kv.IKvQueryExecutor, subs []string, req *entity.CreateFileMappingRequest) error {
	keys := make([]string, 0, len(subs))
	for _, item := range subs {
		keys = append(keys, f.buildKey(item))
	}
	m, err := kv.MultiGetJsonObject[entity.FileMappingItem](ctx, tx, f.table(), keys)
	if err != nil {
		return err
	}
	for _, sub := range subs {
		key := f.buildKey(sub)
		parent, ok := m[key]
		if ok && !parent.IsDirEntry {
			return fmt.Errorf("cant create dir over file, target file:%s, sub file:%s", req.FileName, sub)
		}
		if ok {
			continue
		}
		if err := kv.SetJsonObject(ctx, tx, f.table(), key, &entity.FileMappingItem{
			FileName:   sub,
			FileId:     0,
			FileSize:   0,
			IsDirEntry: true,
			Ctime:      req.Ctime,
			Mtime:      req.Mtime,
			FileMode:   constant.DefaultFileMode,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (f *fileMappingDao) CreateFileMapping(ctx context.Context, req *entity.CreateFileMappingRequest) (*entity.CreateFileMappingResponse, error) {
	if !strings.HasPrefix(req.FileName, "/") {
		return nil, fmt.Errorf("link should starts with '/'")
	}
	if req.IsDir && !strings.HasSuffix(req.FileName, "/") {
		return nil, fmt.Errorf("dir should endswith `/`")
	}
	subs, err := f.resolveSubPaths(req.FileName)
	if err != nil {
		return nil, fmt.Errorf("resolve sub paths failed, err:%w", err)
	}
	item := &entity.FileMappingItem{
		FileName:   req.FileName,
		FileId:     req.FileId,
		Ctime:      req.Ctime,
		Mtime:      req.Mtime,
		FileSize:   req.FileSize,
		IsDirEntry: req.IsDir,
	}
	//创建映射的时候, 递归filepath逐级建立目录, 方便后续处理
	if err := db.GetClient().OnTranscation(ctx, func(ctx context.Context, db kv.IKvQueryExecutor) error {
		if err := f.ensureTargetParent(ctx, db, subs, req); err != nil {
			return err
		}

		return kv.SetJsonObject(ctx, db, f.table(), f.buildKey(req.FileName), item)
	}); err != nil {
		return nil, err
	}
	return &entity.CreateFileMappingResponse{}, nil
}

func (f *fileMappingDao) IterFileMapping(ctx context.Context, prefix string, cb IterFileMappingFunc) error {
	return kv.IterJsonObject(ctx, db.GetClient(), f.table(), defaultFileMappingPrefix+prefix, func(ctx context.Context, key string, val *entity.FileMappingItem) (bool, error) {
		return cb(ctx, val.FileName, val)
	})
}
