package filemgr

import (
	"context"
	"fmt"
	"strconv"

	"github.com/xxxsen/tgfile/directory"
	"github.com/xxxsen/tgfile/entity"
)

const maxFileLinkPageSize = 500

func (d *defaultFileManager) ListFileLinksPage(
	ctx context.Context,
	request FileLinkPageRequest,
) (*FileLinkPageResult, error) {
	if request.Path == "" || request.Limit < 1 || request.Limit > maxFileLinkPageSize {
		return nil, ErrInvalidFileLinkPage
	}
	parent, err := d.StatFileLink(ctx, request.Path)
	if err != nil {
		return nil, err
	}
	if !parent.IsDir {
		return nil, ErrNotDirectory
	}
	if request.Cursor != nil && request.Cursor.ParentEntryID != parent.EntryID {
		return nil, ErrFileLinkCursorStale
	}
	directoryCursor := toDirectoryPageCursor(request.Cursor)
	page, err := d.objectDir.ListPage(
		ctx,
		request.Path,
		directoryCursor,
		uint(request.Limit),
	)
	if err != nil {
		return nil, fmt.Errorf("list file links page: %w", err)
	}
	if page.ParentEntryID != parent.EntryID {
		return nil, ErrFileLinkCursorStale
	}
	result := &FileLinkPageResult{
		ParentEntryID: page.ParentEntryID,
		Items:         make([]*entity.FileLinkMeta, 0, len(page.Entries)),
	}
	for _, entry := range page.Entries {
		item, err := fileLinkFromDirectoryEntry(entry)
		if err != nil {
			return nil, err
		}
		result.Items = append(result.Items, item)
	}
	if page.HasMore && len(result.Items) != 0 {
		last := result.Items[len(result.Items)-1]
		result.NextCursor = &FileLinkPageCursor{
			ParentEntryID: page.ParentEntryID,
			IsDir:         last.IsDir,
			Name:          last.FileName,
			EntryID:       last.EntryID,
		}
	}
	return result, nil
}

func toDirectoryPageCursor(cursor *FileLinkPageCursor) *directory.PageCursor {
	if cursor == nil {
		return nil
	}
	return &directory.PageCursor{
		IsDir:   cursor.IsDir,
		Name:    cursor.Name,
		EntryID: cursor.EntryID,
	}
}

func fileLinkFromDirectoryEntry(entry directory.IDirectoryEntry) (*entity.FileLinkMeta, error) {
	item := &entity.FileLinkMeta{
		EntryID:  entry.EntryID(),
		FileName: entry.Name(),
		FileSize: entry.Size(),
		Mode:     entry.Mode(),
		Ctime:    entry.Ctime(),
		Mtime:    entry.Mtime(),
		IsDir:    entry.IsDir(),
	}
	if item.IsDir {
		return item, nil
	}
	fileID, err := strconv.ParseUint(entry.RefData(), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("parse paged file link id: %w", err)
	}
	item.FileId = fileID
	return item, nil
}
