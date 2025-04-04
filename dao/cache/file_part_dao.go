package cache

import (
	"context"
	"fmt"
	"time"

	"github.com/xxxsen/tgfile/cache"
	"github.com/xxxsen/tgfile/dao"
	"github.com/xxxsen/tgfile/entity"

	"github.com/xxxsen/common/logutil"
	"go.uber.org/zap"
)

var (
	defaultFilePartCacheExpireTime = 7 * 24 * time.Hour
)

type filePartDao struct {
	dao.IFilePartDao
}

func NewFilePartDao(impl dao.IFilePartDao) dao.IFilePartDao {
	return &filePartDao{
		IFilePartDao: impl,
	}
}

func (f *filePartDao) CreateFilePart(ctx context.Context, req *entity.CreateFilePartRequest) (*entity.CreateFilePartResponse, error) {
	//filepartid 能被覆盖, 所以创建后需要清理缓存
	defer cache.Del(ctx, f.buildCacheKey(req.FileId, req.FilePartId))
	return f.IFilePartDao.CreateFilePart(ctx, req)
}

func (f *filePartDao) buildCacheKey(fid uint64, bid int32) string {
	return fmt.Sprintf("tgfile:cache:filepart:%d:%d", fid, bid)
}

func (f *filePartDao) GetFilePartInfo(ctx context.Context, req *entity.GetFilePartInfoRequest) (*entity.GetFilePartInfoResponse, error) {
	ks := make([]string, 0, len(req.FilePartId))
	mapping := make(map[string]int32, len(req.FilePartId))
	for _, bid := range req.FilePartId {
		key := f.buildCacheKey(req.FileId, bid)
		ks = append(ks, key)
		mapping[key] = bid
	}
	cacheRs, err := cache.LoadMany(ctx, ks, func(ctx context.Context, c cache.ICache, ks []string) (map[string]interface{}, error) {
		bids := make([]int32, 0, len(ks))
		for _, k := range ks {
			bids = append(bids, mapping[k])
		}
		rsp, err := f.IFilePartDao.GetFilePartInfo(ctx, &entity.GetFilePartInfoRequest{
			FileId:     req.FileId,
			FilePartId: bids,
		})
		if err != nil {
			return nil, err
		}
		rs := make(map[string]interface{})
		for _, item := range rsp.List {
			key := f.buildCacheKey(req.FileId, item.FilePartId)
			_ = c.Set(ctx, key, item, defaultFilePartCacheExpireTime)
			rs[key] = item
		}
		return rs, nil
	})
	if err != nil {
		return nil, err
	}
	rsp := &entity.GetFilePartInfoResponse{}
	for _, k := range ks {
		bid := mapping[k]
		res, ok := cacheRs[k]
		if !ok {
			logutil.GetLogger(ctx).Error("cache key not found", zap.Uint64("file_id", req.FileId), zap.Int32("file_part_id", bid), zap.String("key", k))
			continue
		}
		rsp.List = append(rsp.List, res.(*entity.FilePartInfoItem))
	}
	return rsp, nil
}
