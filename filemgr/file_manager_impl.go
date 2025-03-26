package filemgr

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"tgfile/blockio"
	"tgfile/dao"
	"tgfile/dao/cache"
	"tgfile/entity"
	"time"

	"github.com/xxxsen/common/logutil"
	"go.uber.org/zap"
)

type defaultFileManager struct {
	fileDao        dao.IFileDao
	filePartDao    dao.IFilePartDao
	fileMappingDao dao.IFileMappingDao
	bkio           blockio.IBlockIO
}

func (d *defaultFileManager) CreateLink(ctx context.Context, link string, fileid uint64, size int64) error {
	_, err := d.fileMappingDao.CreateFileMapping(ctx, &entity.CreateFileMappingRequest{
		FileName: link,
		FileId:   fileid,
		FileSize: size,
		IsDir:    false,
	})
	return err
}

func (d *defaultFileManager) ResolveLink(ctx context.Context, link string) (*entity.FileMappingItem, error) {
	fid, ok, err := d.internalGetFileMapping(ctx, link)
	if err != nil {
		return nil, fmt.Errorf("open mapping failed, err:%w", err)
	}
	if !ok {
		return nil, fmt.Errorf("link not found")
	}
	return fid, nil
}

func (d *defaultFileManager) IterLink(ctx context.Context, prefix string, cb IterLinkFunc) error {
	return d.fileMappingDao.IterFileMapping(ctx, prefix, func(ctx context.Context, name string, ent *entity.FileMappingItem) (bool, error) {
		return cb(ctx, name, ent)
	})
}

func (d *defaultFileManager) Open(ctx context.Context, fileid uint64) (io.ReadSeekCloser, error) {
	finfo, ok, err := d.internalGetFileInfo(ctx, fileid)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("file not found")
	}
	rsc := newFileStream(ctx, d.bkio, func(ctx context.Context, blkid int32) (fk string, err error) {
		defer func() {
			if err != nil {
				logutil.GetLogger(ctx).Error("convert blockid to filekey failed", zap.Error(err), zap.Uint64("file_id", fileid), zap.Int32("blkid", blkid))
			}
		}()
		pinfo, ok, err := d.internalGetFilePartInfo(ctx, fileid, blkid)
		if err != nil {
			return "", fmt.Errorf("read file part info failed, err:%w", err)
		}
		if !ok {
			return "", fmt.Errorf("partid:%d not found", blkid)
		}
		return pinfo.FileKey, nil
	}, finfo.FileSize)
	return rsc, nil
}

func (d *defaultFileManager) internalCalcFileBlockCount(sz uint64, blksz uint64) int {
	return int((sz + blksz - 1) / blksz)
}

func (d *defaultFileManager) Create(ctx context.Context, size int64, reader io.Reader) (uint64, error) {
	blkcnt := d.internalCalcFileBlockCount(uint64(size), uint64(d.bkio.MaxFileSize()))
	fileid, err := d.internalCreateFileDraft(ctx, size, int32(blkcnt))
	if err != nil {
		return 0, fmt.Errorf("create file draft failed, err:%w", err)
	}
	for i := 0; i < blkcnt; i++ {
		r := io.LimitReader(reader, d.bkio.MaxFileSize())
		fileKey, err := d.bkio.Upload(ctx, r)
		if err != nil {
			return 0, fmt.Errorf("upload part failed, err:%w", err)
		}
		if err := d.internalCreateFilePart(ctx, fileid, int32(i), fileKey); err != nil {
			return 0, fmt.Errorf("create part record failed, err:%w", err)
		}
	}

	if err := d.internalFinishCreateFile(ctx, fileid); err != nil {
		return 0, fmt.Errorf("finish create file failed, err:%w", err)
	}
	return fileid, nil
}

func (d *defaultFileManager) Stat(ctx context.Context, fileid uint64) (fs.FileInfo, error) {
	finfo, ok, err := d.internalGetFileInfo(ctx, fileid)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("file not found")
	}
	return &defaultFileInfo{
		FieldSize:  finfo.FileSize,
		FieldMtime: time.UnixMilli(finfo.Mtime),
		FieldName:  "noname",
	}, nil
}

func (d *defaultFileManager) internalCreateFileDraft(ctx context.Context, filesize int64, filepartcount int32) (uint64, error) {
	rs, err := d.fileDao.CreateFileDraft(ctx, &entity.CreateFileDraftRequest{
		FileSize:      filesize,
		FilePartCount: filepartcount,
	})
	if err != nil {
		return 0, err
	}
	return rs.FileId, nil
}

func (d *defaultFileManager) internalCreateFilePart(ctx context.Context, fileid uint64, pidx int32, filekey string) error {
	if _, err := d.filePartDao.CreateFilePart(ctx, &entity.CreateFilePartRequest{
		FileId:     fileid,
		FilePartId: pidx,
		FileKey:    filekey,
	}); err != nil {
		return err
	}
	return nil
}

func (d *defaultFileManager) internalFinishCreateFile(ctx context.Context, fileid uint64) error {
	if _, err := d.fileDao.MarkFileReady(ctx, &entity.MarkFileReadyRequest{
		FileID: fileid,
	}); err != nil {
		return err
	}
	return nil
}

func (d *defaultFileManager) internalGetFileInfo(ctx context.Context, fileid uint64) (*entity.FileInfoItem, bool, error) {
	rs, err := d.fileDao.GetFileInfo(ctx, &entity.GetFileInfoRequest{
		FileIds: []uint64{fileid},
	})
	if err != nil {
		return nil, false, err
	}
	if len(rs.List) == 0 {
		return nil, false, nil
	}
	return rs.List[0], true, nil
}

func (d *defaultFileManager) internalGetFilePartInfo(ctx context.Context, fileid uint64, partid int32) (*entity.FilePartInfoItem, bool, error) {
	rs, err := d.filePartDao.GetFilePartInfo(ctx, &entity.GetFilePartInfoRequest{
		FileId:     fileid,
		FilePartId: []int32{partid},
	})
	if err != nil {
		return nil, false, err
	}
	if len(rs.List) == 0 {
		return nil, false, nil
	}
	return rs.List[0], true, nil
}

func (d *defaultFileManager) internalGetFileMapping(ctx context.Context, filename string) (*entity.FileMappingItem, bool, error) {
	rsp, ok, err := d.fileMappingDao.GetFileMapping(ctx, &entity.GetFileMappingRequest{
		FileName: filename,
	})
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return nil, false, nil
	}
	return rsp.Item, true, nil
}

func NewFileManager(bkio blockio.IBlockIO) IFileManager {
	return &defaultFileManager{
		fileDao:        cache.NewFileDao(dao.NewFileDao()),
		filePartDao:    cache.NewFilePartDao(dao.NewFilePartDao()),
		fileMappingDao: cache.NewFileMappingDao(dao.NewFileMappingDao()),
		bkio:           bkio,
	}
}
