package cache

import (
	"context"
	"time"

	lru "github.com/hashicorp/golang-lru/v2/expirable"
	"github.com/xxxsen/tgfile/cacheapi"
	cachewrap "github.com/xxxsen/tgfile/cacheapi/adaptor"
	"github.com/xxxsen/tgfile/dao"
	"github.com/xxxsen/tgfile/entity"
)

const (
	defaultMaxFileDaoCacheSize    = 10000
	defaultFileDaoCacheExpireTime = 7 * 24 * time.Hour
)

type fileDao struct {
	dao.IFileDao
	cache cacheapi.ICache[uint64, *entity.FileInfoItem]
}

func NewFileDao(impl dao.IFileDao) dao.IFileDao {
	cc := lru.NewLRU[uint64, *entity.FileInfoItem](defaultMaxFileDaoCacheSize, nil, defaultFileDaoCacheExpireTime)
	return &fileDao{
		IFileDao: impl,
		cache:    cachewrap.WrapExpirableLruCache(cc),
	}
}

func (f *fileDao) MarkFileReady(ctx context.Context, req *entity.MarkFileReadyRequest) (*entity.MarkFileReadyResponse, error) {
	defer f.cache.Del(ctx, req.FileID)
	return f.IFileDao.MarkFileReady(ctx, req)
}

func (f *fileDao) GetFileInfo(ctx context.Context, req *entity.GetFileInfoRequest) (*entity.GetFileInfoResponse, error) {
	m, err := cacheapi.LoadMany(ctx, f.cache, req.FileIds, func(ctx context.Context, miss []uint64) (map[uint64]*entity.FileInfoItem, error) {
		res, err := f.IFileDao.GetFileInfo(ctx, &entity.GetFileInfoRequest{
			FileIds: miss,
		})
		if err != nil {
			return nil, err
		}
		rs := make(map[uint64]*entity.FileInfoItem, len(res.List))
		for _, item := range res.List {
			rs[item.FileId] = item
		}
		return rs, nil
	})
	if err != nil {
		return nil, err
	}
	rsp := &entity.GetFileInfoResponse{}
	for _, fid := range req.FileIds {
		v, ok := m[fid]
		if !ok {
			continue
		}
		rsp.List = append(rsp.List, v)
	}
	return rsp, nil
}

func (f *fileDao) DeleteFile(ctx context.Context, req *entity.DeleteFileRequest) (*entity.DeleteFileResponse, error) {
	defer func() {
		for _, fid := range req.FileId {
			_ = f.cache.Del(ctx, fid)
		}
	}()

	return f.IFileDao.DeleteFile(ctx, req)
}
