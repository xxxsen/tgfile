package dao

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strconv"

	"github.com/xxxsen/tgfile/entity"

	"github.com/xxxsen/tgfile/directory"

	"github.com/xxxsen/common/database"
	"github.com/xxxsen/common/idgen"
)

var (
	errDeleteStateRowMissing = errors.New("mapping file delete state query returned no row")
	errFileDeleteNotLive     = errors.New("file has non-live block delete state")
)

type (
	IterFileLinkFunc func(ctx context.Context, name string, ent *entity.FileLinkMeta) (bool, error)
	ScanFileLinkFunc func(ctx context.Context, res []*entity.FileLinkMeta) (bool, error)
)

type IFileMappingReader interface {
	GetFileLinkMeta(
		ctx context.Context,
		req *entity.GetFileLinkMetaRequest,
	) (*entity.GetFileLinkMetaResponse, bool, error)
	IterFileLink(ctx context.Context, prefix string, cb IterFileLinkFunc) error
	ScanFileLink(ctx context.Context, batch uint, cb ScanFileLinkFunc) error
}

type IFileMappingWriter interface {
	CreateFileLink(ctx context.Context, req *entity.CreateFileLinkRequest) (*entity.CreateFileLinkResponse, error)
	RemoveFileLink(ctx context.Context, link string) error
	RenameFileLink(ctx context.Context, src, dst string, isOverwrite bool) error
	CopyFileLink(ctx context.Context, src, dst string, isOverwrite bool) error
}

type IFileMappingDao interface {
	IFileMappingReader
	IFileMappingWriter
}

type fileMappingDaoImpl struct {
	dbc database.IDatabase
	dir directory.ITransactionalDirectory
}

func NewFileMappingDao(dbc database.IDatabase) IFileMappingDao {
	d := &fileMappingDaoImpl{
		dbc: dbc,
	}
	dir, err := directory.NewDBDirectory(dbc, d.table(), idgen.Default().NextId)
	if err != nil {
		panic(err)
	}
	d.dir = dir
	return d
}

func (f *fileMappingDaoImpl) table() string {
	return "tg_file_mapping_tab"
}

func (f *fileMappingDaoImpl) GetFileLinkMeta(
	ctx context.Context,
	req *entity.GetFileLinkMetaRequest,
) (*entity.GetFileLinkMetaResponse, bool, error) {
	ent, err := f.dir.Stat(ctx, req.FileName)
	if err != nil {
		return nil, false, fmt.Errorf("stat file link %q: %w", req.FileName, err)
	}
	item, err := f.directoryEntryToFileMappingItem(ent)
	if err != nil {
		return nil, false, err
	}
	return &entity.GetFileLinkMetaResponse{Item: item}, true, nil
}

func (f *fileMappingDaoImpl) CreateFileLink(
	ctx context.Context,
	req *entity.CreateFileLinkRequest,
) (*entity.CreateFileLinkResponse, error) {
	if req.IsDir {
		if err := f.dir.Mkdir(ctx, req.FileName); err != nil {
			return nil, fmt.Errorf("create directory link %q: %w", req.FileName, err)
		}
		return &entity.CreateFileLinkResponse{}, nil
	}
	if err := f.dir.WithTransaction(ctx, func(ctx context.Context, tx directory.ITransaction) error {
		if err := ensureMappingFileLinkable(ctx, tx.QueryExecer(), req.FileId); err != nil {
			return fmt.Errorf("ensure mapping file is linkable: %w", err)
		}
		_, err := tx.Create(ctx, req.FileName, req.FileSize, fmt.Sprintf("%d", req.FileId))
		if err != nil {
			return fmt.Errorf("create mapping entry: %w", err)
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("create file link %q transaction: %w", req.FileName, err)
	}
	return &entity.CreateFileLinkResponse{}, nil
}

func (f *fileMappingDaoImpl) RemoveFileLink(ctx context.Context, link string) error {
	if err := f.dir.WithTransaction(ctx, func(ctx context.Context, tx directory.ITransaction) error {
		removed, err := tx.Remove(ctx, link)
		if err != nil {
			return fmt.Errorf("remove mapping entry: %w", err)
		}
		return deleteEntryMetadata(ctx, tx.QueryExecer(), removed)
	}); err != nil {
		return fmt.Errorf("remove file link %q: %w", link, err)
	}
	return nil
}

func (f *fileMappingDaoImpl) RenameFileLink(ctx context.Context, src, dst string, isOverwrite bool) error {
	if err := f.dir.WithTransaction(ctx, func(ctx context.Context, tx directory.ITransaction) error {
		overwritten, err := tx.Move(ctx, src, dst, isOverwrite)
		if err != nil {
			return fmt.Errorf("move mapping entry: %w", err)
		}
		return deleteEntryMetadata(ctx, tx.QueryExecer(), overwritten)
	}); err != nil {
		return fmt.Errorf("rename file link %q to %q: %w", src, dst, err)
	}
	return nil
}

func (f *fileMappingDaoImpl) CopyFileLink(ctx context.Context, src, dst string, isOverwrite bool) error {
	if err := f.dir.WithTransaction(ctx, func(ctx context.Context, tx directory.ITransaction) error {
		copies, overwritten, err := tx.Copy(ctx, src, dst, isOverwrite)
		if err != nil {
			return fmt.Errorf("copy mapping entry: %w", err)
		}
		for _, copied := range copies {
			if copied.Source.IsDir() {
				continue
			}
			fileID, err := strconv.ParseUint(copied.Source.RefData(), 10, 64)
			if err != nil {
				return fmt.Errorf("parse copied mapping file id: %w", err)
			}
			if err := ensureMappingFileLinkable(ctx, tx.QueryExecer(), fileID); err != nil {
				return fmt.Errorf("ensure copied mapping file is linkable: %w", err)
			}
		}
		if err := deleteEntryMetadata(ctx, tx.QueryExecer(), overwritten); err != nil {
			return err
		}
		for _, copied := range copies {
			if copied.Source.IsDir() {
				continue
			}
			if err := copyEntryMetadata(
				ctx,
				tx.QueryExecer(),
				copied.Source.EntryID(),
				copied.Destination.EntryID(),
			); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("copy file link %q to %q: %w", src, dst, err)
	}
	return nil
}

func ensureMappingFileLinkable(
	ctx context.Context,
	queryer database.IQueryer,
	fileID uint64,
) error {
	rows, err := queryer.QueryContext(
		ctx,
		`SELECT COUNT(*) FROM tg_file_part_delete_state_tab
WHERE file_id = ? AND delete_state != 'live'`,
		fileID,
	)
	if err != nil {
		return fmt.Errorf("query mapping file delete state: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate mapping file delete state: %w", err)
		}
		return errDeleteStateRowMissing
	}
	var count int64
	if err := rows.Scan(&count); err != nil {
		return fmt.Errorf("scan mapping file delete state: %w", err)
	}
	if count != 0 {
		return errFileDeleteNotLive
	}
	return nil
}

func deleteEntryMetadata(
	ctx context.Context,
	exec database.IExecer,
	entries []directory.IDirectoryEntry,
) error {
	for _, entry := range entries {
		if _, err := exec.ExecContext(
			ctx,
			"DELETE FROM tg_s3_object_metadata_tab WHERE entry_id = ?",
			entry.EntryID(),
		); err != nil {
			return fmt.Errorf("delete mapping metadata: %w", err)
		}
	}
	return nil
}

func copyEntryMetadata(
	ctx context.Context,
	exec database.IExecer,
	sourceEntryID, destinationEntryID uint64,
) error {
	const statement = `INSERT INTO tg_s3_object_metadata_tab (
entry_id, etag, checksum_sha256, request_checksum_algorithm, request_checksum_value,
content_type, cache_control, content_disposition, content_encoding, content_language,
expires, user_metadata, ctime, mtime
)
SELECT ?, etag, checksum_sha256, request_checksum_algorithm, request_checksum_value,
content_type, cache_control, content_disposition, content_encoding, content_language,
expires, user_metadata, ctime, mtime
FROM tg_s3_object_metadata_tab WHERE entry_id = ?`
	if _, err := exec.ExecContext(ctx, statement, destinationEntryID, sourceEntryID); err != nil {
		return fmt.Errorf("copy mapping metadata: %w", err)
	}
	return nil
}

func (f *fileMappingDaoImpl) IterFileLink(ctx context.Context, prefix string, cb IterFileLinkFunc) error {
	ents, err := f.dir.List(ctx, prefix)
	if err != nil {
		return fmt.Errorf("list file links under %q: %w", prefix, err)
	}
	for _, item := range ents {
		cbitem, err := f.directoryEntryToFileMappingItem(item)
		if err != nil {
			return fmt.Errorf("convert file link %q: %w", item.Name(), err)
		}
		next, err := cb(ctx, path.Join(prefix, item.Name()), cbitem)
		if err != nil {
			return fmt.Errorf("process file link %q: %w", item.Name(), err)
		}
		if !next {
			break
		}
	}
	return nil
}

func (f *fileMappingDaoImpl) directoryEntryToFileMappingItem(
	item directory.IDirectoryEntry,
) (*entity.FileLinkMeta, error) {
	rs := &entity.FileLinkMeta{
		EntryID:  item.EntryID(),
		FileName: item.Name(),
		FileId:   0,
		FileSize: item.Size(),
		Mode:     item.Mode(),
		Ctime:    item.Ctime(),
		Mtime:    item.Mtime(),
		IsDir:    item.IsDir(),
	}
	if !rs.IsDir {
		fid, err := strconv.ParseUint(item.RefData(), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse file id %q: %w", item.RefData(), err)
		}
		rs.FileId = fid
	}
	return rs, nil
}

func (f *fileMappingDaoImpl) ScanFileLink(ctx context.Context, batch uint, cb ScanFileLinkFunc) error {
	err := f.dir.Scan(ctx, batch, func(ctx context.Context, res []directory.IDirectoryEntry) (bool, error) {
		cbitems := make([]*entity.FileLinkMeta, 0, len(res))
		for _, item := range res {
			cbitem, err := f.directoryEntryToFileMappingItem(item)
			if err != nil {
				return false, fmt.Errorf("convert scanned file link %q: %w", item.Name(), err)
			}
			cbitems = append(cbitems, cbitem)
		}
		return cb(ctx, cbitems)
	})
	if err != nil {
		return fmt.Errorf("scan file links: %w", err)
	}
	return nil
}
