package filemgr

import (
	"context"
	"crypto/md5" //nolint:gosec // Persisted legacy checksum format; not used for security.
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"time"

	"github.com/xxxsen/tgfile/blockio"
	"github.com/xxxsen/tgfile/dao"
	"github.com/xxxsen/tgfile/dao/cache"
	"github.com/xxxsen/tgfile/directory"
	"github.com/xxxsen/tgfile/entity"

	"github.com/xxxsen/common/database"
	"github.com/xxxsen/common/idgen"
	"github.com/xxxsen/common/logutil"
	"go.uber.org/zap"
)

var errInvalidUploadDeleteReference = errors.New("upload part returned an invalid deletion reference")

// MD5CompatibilitySize is the digest size required by persisted and S3 compatibility fields.
const MD5CompatibilitySize = md5.Size

// NewMD5CompatibilityHash returns the protocol compatibility hash used by legacy storage and S3.
func NewMD5CompatibilityHash() hash.Hash {
	return md5.New() //nolint:gosec // Persisted legacy and S3 protocol fields require MD5 compatibility.
}

type defaultFileManager struct {
	fileDao        dao.IFileDao
	filePartDao    dao.IFilePartDao
	fileMappingDao dao.IFileMappingDao
	dbc            database.IDatabase
	objectDir      directory.ITransactionalDirectory
	bkio           blockio.IBlockIO
	ioc            IFileIOCache
}

const maxFilePartCount int64 = 100_000

type countingReader struct {
	reader io.Reader
	count  int64
}

func (c *countingReader) Read(buffer []byte) (int, error) {
	read, err := c.reader.Read(buffer)
	c.count += int64(read)
	if err != nil && err != io.EOF {
		return read, fmt.Errorf("read counted file content: %w", err)
	}
	if err == io.EOF {
		return read, io.EOF
	}
	return read, nil
}

func (d *defaultFileManager) CreateFileLink(
	ctx context.Context,
	link string,
	fileid uint64,
	size int64,
	isDir bool,
) error {
	_, err := d.fileMappingDao.CreateFileLink(ctx, &entity.CreateFileLinkRequest{
		FileName: link,
		FileId:   fileid,
		FileSize: size,
		IsDir:    isDir,
	})
	if err != nil {
		return fmt.Errorf("create file link %q: %w", link, err)
	}
	return nil
}

func (d *defaultFileManager) StatFileLink(ctx context.Context, link string) (*entity.FileLinkMeta, error) {
	fid, ok, err := d.internalGetFileMapping(ctx, link)
	if err != nil {
		return nil, fmt.Errorf("open mapping failed, err:%w", err)
	}
	if !ok {
		return nil, fmt.Errorf("link not found: %w", os.ErrNotExist)
	}
	return fid, nil
}

func (d *defaultFileManager) WalkFileLink(ctx context.Context, prefix string, cb WalkLinkFunc) error {
	callback := func(
		ctx context.Context,
		name string,
		entry *entity.FileLinkMeta,
	) (bool, error) {
		return cb(ctx, name, entry)
	}
	err := d.fileMappingDao.IterFileLink(ctx, prefix, callback)
	if err != nil {
		return fmt.Errorf("walk file links under %q: %w", prefix, err)
	}
	return nil
}

func (d *defaultFileManager) RemoveFileLink(ctx context.Context, link string) error {
	if err := d.fileMappingDao.RemoveFileLink(ctx, link); err != nil {
		return fmt.Errorf("remove file link %q: %w", link, err)
	}
	return nil
}

func (d *defaultFileManager) RenameFileLink(ctx context.Context, src, dst string, isOverwrite bool) error {
	if err := d.fileMappingDao.RenameFileLink(ctx, src, dst, isOverwrite); err != nil {
		return fmt.Errorf("rename file link %q to %q: %w", src, dst, err)
	}
	return nil
}

func (d *defaultFileManager) CopyFileLink(ctx context.Context, src, dst string, isOverwrite bool) error {
	if err := d.fileMappingDao.CopyFileLink(ctx, src, dst, isOverwrite); err != nil {
		return fmt.Errorf("copy file link %q to %q: %w", src, dst, err)
	}
	return nil
}

func (d *defaultFileManager) lowlevelIOStream(
	bkio blockio.IBlockIO,
	fileid uint64,
	filesize int64,
) func(ctx context.Context) (io.ReadSeekCloser, error) {
	return func(ctx context.Context) (io.ReadSeekCloser, error) {
		return newFileStream(ctx, bkio, func(ctx context.Context, blkid int32) (string, error) {
			pinfo, ok, err := d.internalGetFilePartInfo(ctx, fileid, blkid)
			if err != nil {
				logutil.GetLogger(ctx).Error(
					"convert blockid to filekey failed",
					zap.Error(err),
					zap.Uint64("file_id", fileid),
					zap.Int32("blkid", blkid),
				)
				return "", fmt.Errorf("read file part info failed, err:%w", err)
			}
			if !ok {
				return "", fmt.Errorf("%w: %d", ErrFilePartNotFound, blkid)
			}
			return pinfo.FileKey, nil
		}, filesize), nil
	}
}

func (d *defaultFileManager) StatFile(ctx context.Context, fileid uint64) (*entity.FileMeta, error) {
	finfo, ok, err := d.internalGetFileInfo(ctx, fileid)
	if err != nil {
		return nil, fmt.Errorf("stat file %d: %w", fileid, err)
	}
	if !ok {
		return nil, os.ErrNotExist
	}
	return finfo.ToFileMeta(), nil
}

func (d *defaultFileManager) OpenFile(ctx context.Context, fileid uint64) (io.ReadSeekCloser, error) {
	finfo, ok, err := d.internalGetFileInfo(ctx, fileid)
	if err != nil {
		return nil, fmt.Errorf("open file %d metadata: %w", fileid, err)
	}
	if !ok {
		return nil, os.ErrNotExist
	}
	rsc, err := d.ioc.Load(ctx, fileid, finfo.FileSize, d.lowlevelIOStream(d.bkio, fileid, finfo.FileSize))
	if err != nil {
		return nil, fmt.Errorf("open file %d content: %w", fileid, err)
	}
	return rsc, nil
}

func calculateFileBlockCount(size, blockSize int64) (int64, error) {
	if size < 0 {
		return 0, fmt.Errorf("%w: %d", ErrInvalidFileSize, size)
	}
	if blockSize <= 0 {
		return 0, fmt.Errorf("%w: %d", ErrInvalidBlockSize, blockSize)
	}
	if size == 0 {
		return 0, nil
	}
	count := 1 + (size-1)/blockSize
	if count > maxFilePartCount {
		return 0, fmt.Errorf("%w: %d exceeds %d", ErrTooManyFileParts, count, maxFilePartCount)
	}
	return count, nil
}

func (d *defaultFileManager) CreateFileDraft(ctx context.Context, size int64) (uint64, int64, error) {
	blockSize := d.bkio.MaxFileSize()
	blockCount, err := calculateFileBlockCount(size, blockSize)
	if err != nil {
		return 0, 0, err
	}

	rs, err := d.fileDao.CreateFileDraft(ctx, &entity.CreateFileDraftRequest{
		FileSize:      size,
		FilePartCount: int32(blockCount), //nolint:gosec // calculateFileBlockCount caps this at 100,000.
	})
	if err != nil {
		return 0, 0, fmt.Errorf("create file draft: %w", err)
	}
	return rs.FileId, blockSize, nil
}

func (d *defaultFileManager) CreateFilePart(ctx context.Context, fileid uint64, partid int64, r io.Reader) error {
	if partid < 0 || partid > maxFilePartCount {
		return fmt.Errorf("%w: %d", ErrInvalidFilePart, partid)
	}
	md5v := NewMD5CompatibilityHash()
	r = io.TeeReader(r, md5v)
	upload, err := d.bkio.Upload(ctx, r)
	if err != nil {
		return fmt.Errorf("upload part failed, err:%w", err)
	}
	if upload == nil || upload.FileKey == "" || upload.DeleteRef == "" || upload.UploadedAt <= 0 {
		return errInvalidUploadDeleteReference
	}
	if _, err := d.filePartDao.CreateFilePart(ctx, &entity.CreateFilePartRequest{
		FileId:      fileid,
		FilePartId:  int32(partid),
		FileKey:     upload.FileKey,
		FilePartMd5: hex.EncodeToString(md5v.Sum(nil)),
		BackendKind: d.bkio.Name(),
		DeleteRef:   upload.DeleteRef,
		UploadedAt:  upload.UploadedAt,
	}); err != nil {
		compensationContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), deleteTimeout)
		deleteErr := d.bkio.DeleteBlocks(compensationContext, []string{upload.DeleteRef})
		cancel()
		if deleteErr != nil {
			logutil.GetLogger(ctx).Error(
				"compensate uploaded block failed",
				zap.Error(deleteErr),
				zap.Uint64("file_id", fileid),
				zap.Int64("part_id", partid),
			)
		}
		return fmt.Errorf("create file part record: %w", err)
	}
	return nil
}

func (d *defaultFileManager) FinishFileCreate(ctx context.Context, fileid uint64) error {
	// 从filepart list中抽取所有的filekey, 基于filekey构建md5
	fps, err := d.filePartDao.ListFilePart(ctx, &entity.ListFilePartRequest{
		FileId: fileid,
	})
	if err != nil {
		return fmt.Errorf("list file parts: %w", err)
	}
	md5v := ""
	if len(fps.List) == 1 {
		md5v = fps.List[0].FilePartMd5
	}
	if len(fps.List) > 1 {
		h := NewMD5CompatibilityHash()
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
		return fmt.Errorf("encode file extension info: %w", err)
	}

	if _, err := d.fileDao.MarkFileReady(ctx, &entity.MarkFileReadyRequest{
		FileID:  fileid,
		Extinfo: string(raw),
	}); err != nil {
		return fmt.Errorf("mark file ready: %w", err)
	}
	return nil
}

func (d *defaultFileManager) CreateFile(ctx context.Context, size int64, reader io.Reader) (uint64, error) {
	fileid, blksize, err := d.CreateFileDraft(ctx, size)
	if err != nil {
		return 0, err
	}
	blockCount, err := calculateFileBlockCount(size, blksize)
	if err != nil {
		return 0, err
	}
	var uploadedSize int64
	for partID := int64(0); partID < blockCount; partID++ {
		partSize := min(blksize, size-uploadedSize)
		counted := &countingReader{reader: io.LimitReader(reader, partSize)}
		if err := d.CreateFilePart(ctx, fileid, partID, counted); err != nil {
			return 0, fmt.Errorf("create part record failed, err:%w", err)
		}
		if counted.count != partSize {
			return 0, fmt.Errorf("%w: part=%d expected=%d actual=%d", ErrFileShortRead, partID, partSize, counted.count)
		}
		uploadedSize += counted.count
	}
	if err := d.FinishFileCreate(ctx, fileid); err != nil {
		return 0, fmt.Errorf("finish create file failed, err:%w", err)
	}
	return fileid, nil
}

func (d *defaultFileManager) internalGetFileInfo(
	ctx context.Context,
	fileid uint64,
) (*entity.FileInfoItem, bool, error) {
	rs, err := d.fileDao.GetFileInfo(ctx, &entity.GetFileInfoRequest{
		FileIds: []uint64{fileid},
	})
	if err != nil {
		return nil, false, fmt.Errorf("query file %d: %w", fileid, err)
	}
	if len(rs.List) == 0 {
		return nil, false, nil
	}
	return rs.List[0], true, nil
}

func (d *defaultFileManager) internalGetFilePartInfo(
	ctx context.Context,
	fileid uint64,
	partid int32,
) (*entity.FilePartInfoItem, bool, error) {
	rs, err := d.filePartDao.GetFilePartInfo(ctx, &entity.GetFilePartInfoRequest{
		FileId:     fileid,
		FilePartId: []int32{partid},
	})
	if err != nil {
		return nil, false, fmt.Errorf("query file %d part %d: %w", fileid, partid, err)
	}
	if len(rs.List) == 0 {
		return nil, false, nil
	}
	return rs.List[0], true, nil
}

func (d *defaultFileManager) internalGetFileMapping(
	ctx context.Context,
	filename string,
) (*entity.FileLinkMeta, bool, error) {
	rsp, ok, err := d.fileMappingDao.GetFileLinkMeta(ctx, &entity.GetFileLinkMetaRequest{
		FileName: filename,
	})
	if err != nil {
		return nil, false, fmt.Errorf("query file link %q: %w", filename, err)
	}
	if !ok {
		return nil, false, nil
	}
	return rsp.Item, true, nil
}

func (d *defaultFileManager) cleanUnRefFileIdList(ctx context.Context, fidlist []uint64) (int64, error) {
	var cleaned int64
	for _, fid := range fidlist {
		hasDeleteState, err := d.fileHasDeleteState(ctx, fid)
		if err != nil {
			return cleaned, err
		}
		if hasDeleteState {
			continue
		}
		if _, err := d.filePartDao.DeleteFilePart(ctx, &entity.DeleteFilePartRequest{FileId: []uint64{fid}}); err != nil {
			return cleaned, fmt.Errorf("delete parts for file %d: %w", fid, err)
		}
		if _, err := d.fileDao.DeleteFile(ctx, &entity.DeleteFileRequest{FileId: []uint64{fid}}); err != nil {
			return cleaned, fmt.Errorf("delete file %d: %w", fid, err)
		}
		cleaned++
		logutil.GetLogger(ctx).Info("purge file succ", zap.Uint64("file_id", fid))
	}
	return cleaned, nil
}

func (d *defaultFileManager) fileHasDeleteState(ctx context.Context, fileID uint64) (bool, error) {
	var count int64
	if err := queryRow(
		ctx,
		d.dbc,
		"SELECT COUNT(*) FROM tg_file_part_delete_state_tab WHERE file_id = ?",
		fileID,
	).Scan(&count); err != nil {
		return false, fmt.Errorf("count file delete states: %w", err)
	}
	return count != 0, nil
}

func (d *defaultFileManager) readUnRefFileIdList(ctx context.Context, limitMtime int64) ([]uint64, error) {
	const defaultBatch uint = 2000
	fidMap := make(map[uint64]struct{}, 64)
	if err := d.fileDao.ScanFile(ctx, defaultBatch, func(_ context.Context, res []*entity.FileInfoItem) (bool, error) {
		for _, item := range res {
			if item.Mtime >= limitMtime {
				continue
			}
			fidMap[item.FileId] = struct{}{}
		}
		return true, nil
	}); err != nil {
		return nil, fmt.Errorf("scan files for unreferenced ids: %w", err)
	}
	if len(fidMap) == 0 {
		return nil, nil
	}
	removeReferences := func(_ context.Context, res []*entity.FileLinkMeta) (bool, error) {
		for _, item := range res {
			delete(fidMap, item.FileId)
		}
		return true, nil
	}
	if err := d.fileMappingDao.ScanFileLink(ctx, defaultBatch, removeReferences); err != nil {
		return nil, fmt.Errorf("scan links for referenced file ids: %w", err)
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
	limitMtime := time.Now().AddDate(0, 0, -1).UnixMilli() // mtime 在一天之前的数据才进行清理
	if before != nil {
		limitMtime = *before
	}
	fidList, err := d.readUnRefFileIdList(ctx, limitMtime)
	if err != nil {
		return 0, fmt.Errorf("read un-ref fid list failed, err:%w", err)
	}
	cleaned, err := d.cleanUnRefFileIdList(ctx, fidList)
	if err != nil {
		return 0, fmt.Errorf("clean un-ref fid list failed, err:%w", err)
	}
	return cleaned, nil
}

func NewFileManager(dbc database.IDatabase, bkio blockio.IBlockIO, ioc IFileIOCache) IFileManager {
	objectDir, err := directory.NewDBDirectory(dbc, "tg_file_mapping_tab", idgen.Default().NextId)
	if err != nil {
		panic(err)
	}
	return &defaultFileManager{
		fileDao:        cache.NewFileDao(dao.NewFileDao(dbc)),
		filePartDao:    cache.NewFilePartDao(dao.NewFilePartDao(dbc)),
		fileMappingDao: dao.NewFileMappingDao(dbc),
		dbc:            dbc,
		objectDir:      objectDir,
		bkio:           bkio,
		ioc:            ioc,
	}
}
