package cache

import (
	"context"
	"fmt"
	"time"

	"github.com/xxxsen/tgfile/cache"
	"github.com/xxxsen/tgfile/dao"
	"github.com/xxxsen/tgfile/entity"
)

const (
	defaultFileDaoCacheExpireTime = 7 * 24 * time.Hour
)

type fileDao struct {
	dao.IFileDao
}

func NewFileDao(impl dao.IFileDao) dao.IFileDao {
	return &fileDao{
		IFileDao: impl,
	}
}

func (f *fileDao) MarkFileReady(ctx context.Context, req *entity.MarkFileReadyRequest) (*entity.MarkFileReadyResponse, error) {
	defer cache.Del(ctx, f.buildCacheKey(req.FileID))
	return f.IFileDao.MarkFileReady(ctx, req)
}

func (f *fileDao) buildCacheKey(fid uint64) string {
	return fmt.Sprintf("tgfile:cache:fileid:%d", fid)
}

func (f *fileDao) GetFileInfo(ctx context.Context, req *entity.GetFileInfoRequest) (*entity.GetFileInfoResponse, error) {
	keys := make([]string, 0, len(req.FileIds))
	mapping := make(map[string]uint64, len(req.FileIds))
	for _, fid := range req.FileIds {
		key := f.buildCacheKey(fid)
		keys = append(keys, key)
		mapping[key] = fid
	}
	cacheRs, err := cache.LoadMany(ctx, keys, func(ctx context.Context, c cache.ICache, ks []string) (map[string]interface{}, error) {
		fids := make([]uint64, 0, len(ks))
		for _, k := range ks {
			fids = append(fids, mapping[k])
		}
		rs, err := f.IFileDao.GetFileInfo(ctx, &entity.GetFileInfoRequest{
			FileIds: fids,
		})
		if err != nil {
			return nil, err
		}
		ret := make(map[string]interface{}, len(rs.List))
		for _, item := range rs.List {
			k := f.buildCacheKey(item.FileId)
			ret[k] = item
			_ = c.Set(ctx, k, item, defaultFileDaoCacheExpireTime)
		}
		return ret, nil
	})
	if err != nil {
		return nil, err
	}
	rsp := &entity.GetFileInfoResponse{}
	for _, fid := range req.FileIds {
		v, ok := cacheRs[f.buildCacheKey(fid)]
		if !ok {
			continue
		}
		rsp.List = append(rsp.List, v.(*entity.FileInfoItem))
	}
	return rsp, nil
}
