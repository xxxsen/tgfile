package cache

import (
	"context"
	"fmt"
	"tgfile/cache"
	"tgfile/dao"
	"tgfile/entity"
	"time"
)

var (
	defaultFileMappingCacheExpireTime = 7 * 24 * time.Hour
)

type fileMappingDao struct {
	impl dao.IFileMappingDao
}

func NewFileMappingDao(impl dao.IFileMappingDao) dao.IFileMappingDao {
	return &fileMappingDao{impl: impl}
}

func (f *fileMappingDao) buildCacheKey(fname string) string {
	return fmt.Sprintf("tgfile:cache:filename:%s", fname)
}

func (f *fileMappingDao) GetFileMapping(ctx context.Context, req *entity.GetFileMappingRequest) (*entity.GetFileMappingResponse, bool, error) {
	key := f.buildCacheKey(req.FileName)
	res, ok, err := cache.Load(ctx, key, func(ctx context.Context, c cache.ICache, k string) (interface{}, bool, error) {
		rsp, ok, err := f.impl.GetFileMapping(ctx, req)
		if err != nil || !ok {
			return nil, ok, err
		}
		_ = c.Set(ctx, k, rsp.Item, defaultFileMappingCacheExpireTime)
		return rsp.Item, true, nil
	})
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return nil, false, nil
	}
	return &entity.GetFileMappingResponse{Item: res.(*entity.FileMappingItem)}, true, nil
}

func (f *fileMappingDao) CreateFileMapping(ctx context.Context, req *entity.CreateFileMappingRequest) (*entity.CreateFileMappingResponse, error) {
	defer cache.Del(ctx, f.buildCacheKey(req.FileName))
	return f.impl.CreateFileMapping(ctx, req)
}

func (f *fileMappingDao) IterFileMapping(ctx context.Context, prefix string, cb dao.IterFileMappingFunc) error {
	return f.impl.IterFileMapping(ctx, prefix, cb)
}
