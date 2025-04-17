package filemgr

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/xxxsen/tgfile/blockio"
	"github.com/xxxsen/tgfile/dao"
	"github.com/xxxsen/tgfile/dao/cache"
	"github.com/xxxsen/tgfile/entity"

	"github.com/xxxsen/common/database"
	"github.com/xxxsen/common/logutil"
	"go.uber.org/zap"
)

type defaultFileManager struct {
	fileDao        dao.IFileDao
	filePartDao    dao.IFilePartDao
	fileMappingDao dao.IFileMappingDao
	bkio           blockio.IBlockIO
	ioc            IFileIOCache
}

func (d *defaultFileManager) CreateFileLink(ctx context.Context, link string, fileid uint64, size int64, isDir bool) error {
	_, err := d.fileMappingDao.CreateFileLink(ctx, &entity.CreateFileLinkRequest{
		FileName: link,
		FileId:   fileid,
		FileSize: size,
		IsDir:    isDir,
	})
	return err
}

func (d *defaultFileManager) StatFileLink(ctx context.Context, link string) (*entity.FileLinkMeta, error) {
	fid, ok, err := d.internalGetFileMapping(ctx, link)
	if err != nil {
		return nil, fmt.Errorf("open mapping failed, err:%w", err)
	}
	if !ok {
		return nil, fmt.Errorf("link not found")
	}
	return fid, nil
}

func (d *defaultFileManager) WalkFileLink(ctx context.Context, prefix string, cb WalkLinkFunc) error {
	return d.fileMappingDao.IterFileLink(ctx, prefix, func(ctx context.Context, name string, ent *entity.FileLinkMeta) (bool, error) {
		return cb(ctx, name, ent)
	})
}

func (d *defaultFileManager) RemoveFileLink(ctx context.Context, link string) error {
	return d.fileMappingDao.RemoveFileLink(ctx, link)
}

func (d *defaultFileManager) RenameFileLink(ctx context.Context, src, dst string, isOverwrite bool) error {
	return d.fileMappingDao.RenameFileLink(ctx, src, dst, isOverwrite)
}

func (d *defaultFileManager) CopyFileLink(ctx context.Context, src, dst string, isOverwrite bool) error {
	return d.fileMappingDao.CopyFileLink(ctx, src, dst, isOverwrite)
}

func (d *defaultFileManager) lowlevelIOStream(bkio blockio.IBlockIO, fileid uint64, filesize int64) func(ctx context.Context) (io.ReadSeekCloser, error) {
	return func(ctx context.Context) (io.ReadSeekCloser, error) {
		return newFileStream(ctx, bkio, func(ctx context.Context, blkid int32) (fk string, err error) {
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
		}, filesize), nil
	}
}

func (d *defaultFileManager) StatFile(ctx context.Context, fileid uint64) (*entity.FileMeta, error) {
	finfo, ok, err := d.internalGetFileInfo(ctx, fileid)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, os.ErrNotExist
	}
	return finfo.ToFileMeta(), nil
}

func (d *defaultFileManager) OpenFile(ctx context.Context, fileid uint64) (io.ReadSeekCloser, error) {
	finfo, ok, err := d.internalGetFileInfo(ctx, fileid)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, os.ErrNotExist
	}
	rsc, err := d.ioc.Load(ctx, fileid, finfo.FileSize, d.lowlevelIOStream(d.bkio, fileid, finfo.FileSize))
	if err != nil {
		return nil, err
	}
	return rsc, nil
}

func (d *defaultFileManager) internalCalcFileBlockCount(sz uint64, blksz uint64) int {
	return int((sz + blksz - 1) / blksz)
}

func (d *defaultFileManager) CreateFileDraft(ctx context.Context, size int64) (uint64, int64, error) {
	blkcnt := d.internalCalcFileBlockCount(uint64(size), uint64(d.bkio.MaxFileSize()))

	rs, err := d.fileDao.CreateFileDraft(ctx, &entity.CreateFileDraftRequest{
		FileSize:      size,
		FilePartCount: int32(blkcnt),
	})
	if err != nil {
		return 0, 0, err
	}
	return rs.FileId, d.bkio.MaxFileSize(), nil
}

func (d *defaultFileManager) CreateFilePart(ctx context.Context, fileid uint64, partid int64, r io.Reader) error {
	md5v := md5.New()
	r = io.TeeReader(r, md5v)
	fileKey, err := d.bkio.Upload(ctx, r)
	if err != nil {
		return fmt.Errorf("upload part failed, err:%w", err)
	}
	if _, err := d.filePartDao.CreateFilePart(ctx, &entity.CreateFilePartRequest{
		FileId:      fileid,
		FilePartId:  int32(partid),
		FileKey:     fileKey,
		FilePartMd5: hex.EncodeToString(md5v.Sum(nil)),
	}); err != nil {
		return err
	}
	return nil
}

func (d *defaultFileManager) FinishFileCreate(ctx context.Context, fileid uint64) error {
	//从filepart list中抽取所有的filekey, 基于filekey构建md5
	fps, err := d.filePartDao.ListFilePart(ctx, &entity.ListFilePartRequest{
		FileId: fileid,
	})
	if err != nil {
		return err
	}
	var md5v = ""
	if len(fps.List) == 1 {
		md5v = fps.List[0].FilePartMd5
	}
	if len(fps.List) > 1 {
		h := md5.New()
		for _, item := range fps.List {
			_, _ = h.Write([]byte(item.FilePartMd5))
		}
		md5v = hex.EncodeToString(h.Sum(nil))
	}
	ext := &entity.FileExtInfo{
		Md5: md5v,
	}
	raw, err := json.Marshal(ext)
	if err != nil {
		return err
	}

	if _, err := d.fileDao.MarkFileReady(ctx, &entity.MarkFileReadyRequest{
		FileID:  fileid,
		Extinfo: string(raw),
	}); err != nil {
		return err
	}
	return nil
}

func (d *defaultFileManager) CreateFile(ctx context.Context, size int64, reader io.Reader) (uint64, error) {
	fileid, blksize, err := d.CreateFileDraft(ctx, size)
	if err != nil {
		return 0, err
	}
	if blksize == 0 {
		return 0, fmt.Errorf("invalid block size:0")
	}
	blkcnt := d.internalCalcFileBlockCount(uint64(size), uint64(blksize))
	for i := 0; i < blkcnt; i++ {
		r := io.LimitReader(reader, d.bkio.MaxFileSize())
		if err := d.CreateFilePart(ctx, fileid, int64(i), r); err != nil {
			return 0, fmt.Errorf("create part record failed, err:%w", err)
		}
	}
	if err := d.FinishFileCreate(ctx, fileid); err != nil {
		return 0, fmt.Errorf("finish create file failed, err:%w", err)
	}
	return fileid, nil
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

func (d *defaultFileManager) internalGetFileMapping(ctx context.Context, filename string) (*entity.FileLinkMeta, bool, error) {
	rsp, ok, err := d.fileMappingDao.GetFileLinkMeta(ctx, &entity.GetFileLinkMetaRequest{
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

func (d *defaultFileManager) cleanUnRefFileIdList(ctx context.Context, fidlist []uint64) error {
	for _, fid := range fidlist {
		if _, err := d.filePartDao.DeleteFilePart(ctx, &entity.DeleteFilePartRequest{FileId: []uint64{fid}}); err != nil {
			return err
		}
		if _, err := d.fileDao.DeleteFile(ctx, &entity.DeleteFileRequest{FileId: []uint64{fid}}); err != nil {
			return err
		}
		logutil.GetLogger(ctx).Info("purge file succ", zap.Uint64("file_id", fid))
	}
	return nil
}

func (d *defaultFileManager) readUnRefFileIdList(ctx context.Context, limitMtime int64) ([]uint64, error) {
	var defaultBatch int64 = 2000
	fidMap := make(map[uint64]struct{}, 64)
	if err := d.fileDao.ScanFile(ctx, defaultBatch, func(ctx context.Context, res []*entity.FileInfoItem) (bool, error) {
		for _, item := range res {
			if item.Mtime >= limitMtime {
				continue
			}
			fidMap[item.FileId] = struct{}{}
		}
		return true, nil
	}); err != nil {
		return nil, err
	}
	if len(fidMap) == 0 {
		return nil, nil
	}
	if err := d.fileMappingDao.ScanFileLink(ctx, defaultBatch, func(ctx context.Context, res []*entity.FileLinkMeta) (bool, error) {
		for _, item := range res {
			delete(fidMap, item.FileId)
		}
		return true, nil
	}); err != nil {
		return nil, err
	}
	if len(fidMap) == 0 {
		return nil, nil
	}
	rs := make([]uint64, 0, len(fidMap))
	for fid := range fidMap {
		rs = append(rs, fid)
	}
	return rs, nil
}

func (d *defaultFileManager) PurgeFile(ctx context.Context, before *int64) (int64, error) {
	limitMtime := time.Now().AddDate(0, 0, -1).UnixMilli() //mtime 在一天之前的数据才进行清理
	if before != nil {
		limitMtime = *before
	}
	fidList, err := d.readUnRefFileIdList(ctx, limitMtime)
	if err != nil {
		return 0, fmt.Errorf("read un-ref fid list failed, err:%w", err)
	}
	if err := d.cleanUnRefFileIdList(ctx, fidList); err != nil {
		return 0, fmt.Errorf("clean un-ref fid list failed, err:%w", err)
	}
	return int64(len(fidList)), nil
}

func NewFileManager(dbc database.IDatabase, bkio blockio.IBlockIO, ioc IFileIOCache) IFileManager {
	return &defaultFileManager{
		fileDao:        cache.NewFileDao(dao.NewFileDao(dbc)),
		filePartDao:    cache.NewFilePartDao(dao.NewFilePartDao(dbc)),
		fileMappingDao: dao.NewFileMappingDao(dbc),
		bkio:           bkio,
		ioc:            ioc,
	}
}
