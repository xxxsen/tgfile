package cache

import (
	"context"
	"time"

	lru "github.com/hashicorp/golang-lru/v2/expirable"
	"github.com/xxxsen/tgfile/cacheapi"
	cachewrap "github.com/xxxsen/tgfile/cacheapi/adaptor"
	"github.com/xxxsen/tgfile/dao"
	"github.com/xxxsen/tgfile/entity"

	"github.com/xxxsen/common/logutil"
	"go.uber.org/zap"
)

var (
	defaultMaxFilePartCacheSize    = 20000
	defaultFilePartCacheExpireTime = 7 * 24 * time.Hour
)

type filePartPair struct {
	FileId uint64
	PartId int32
}

type filePartDao struct {
	dao.IFilePartDao
	cache cacheapi.ICache[filePartPair, *entity.FilePartInfoItem]
}

func NewFilePartDao(impl dao.IFilePartDao) dao.IFilePartDao {
	cc := lru.NewLRU[filePartPair, *entity.FilePartInfoItem](defaultMaxFilePartCacheSize, nil, defaultFilePartCacheExpireTime)
	return &filePartDao{
		IFilePartDao: impl,
		cache:        cachewrap.WrapExpirableLruCache(cc),
	}
}

func (f *filePartDao) CreateFilePart(ctx context.Context, req *entity.CreateFilePartRequest) (*entity.CreateFilePartResponse, error) {
	//filepartid 能被覆盖, 所以创建后需要清理缓存
	defer f.cache.Del(ctx, filePartPair{FileId: req.FileId, PartId: req.FilePartId})
	return f.IFilePartDao.CreateFilePart(ctx, req)
}

func (f *filePartDao) GetFilePartInfo(ctx context.Context, req *entity.GetFilePartInfoRequest) (*entity.GetFilePartInfoResponse, error) {
	ks := make([]filePartPair, 0, len(req.FilePartId))
	for _, item := range req.FilePartId {
		ks = append(ks, filePartPair{
			FileId: req.FileId,
			PartId: item,
		})
	}
	m, err := cacheapi.LoadMany(ctx, f.cache, ks, func(ctx context.Context, miss []filePartPair) (map[filePartPair]*entity.FilePartInfoItem, error) {
		subreq := &entity.GetFilePartInfoRequest{
			FileId: miss[0].FileId,
		}
		for _, item := range miss {
			subreq.FilePartId = append(subreq.FilePartId, item.PartId)
		}
		subrsp, err := f.IFilePartDao.GetFilePartInfo(ctx, subreq)
		if err != nil {
			return nil, err
		}
		rs := make(map[filePartPair]*entity.FilePartInfoItem, len(subrsp.List))
		for _, item := range subrsp.List {
			rs[filePartPair{FileId: item.FileId, PartId: item.FilePartId}] = item
		}
		return rs, nil
	})
	if err != nil {
		return nil, err
	}
	rsp := &entity.GetFilePartInfoResponse{}
	for _, k := range ks {
		res, ok := m[k]
		if !ok {
			logutil.GetLogger(ctx).Error("cache key not found", zap.Uint64("file_id", req.FileId), zap.Int32("file_part_id", k.PartId))
			continue
		}
		rsp.List = append(rsp.List, res)
	}
	return rsp, nil
}
