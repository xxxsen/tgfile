package filemgr

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestListFileLinksPageAndStaleDirectoryCursor(t *testing.T) {
	manager, _, _ := newCreateFileTestManager(t, 4)
	require.NoError(t, manager.CreateFileLink(t.Context(), "/page", 0, 0, true))
	for _, name := range []string{"z-dir", "A-dir"} {
		require.NoError(t, manager.CreateFileLink(t.Context(), "/page/"+name, 0, 0, true))
	}
	for _, name := range []string{"z.txt", "A.txt", "空.txt"} {
		fileID, err := manager.CreateFile(t.Context(), 1, bytes.NewBufferString("x"))
		require.NoError(t, err)
		require.NoError(t, manager.CreateFileLink(t.Context(), "/page/"+name, fileID, 1, false))
	}

	var (
		cursor *FileLinkPageCursor
		names  []string
	)
	for {
		page, err := manager.ListFileLinksPage(t.Context(), FileLinkPageRequest{
			Path: "/page", Cursor: cursor, Limit: 2,
		})
		require.NoError(t, err)
		for _, item := range page.Items {
			kind := "file:"
			if item.IsDir {
				kind = "dir:"
			}
			names = append(names, kind+item.FileName)
		}
		cursor = page.NextCursor
		if cursor == nil {
			break
		}
	}
	require.Equal(t, []string{
		"dir:A-dir",
		"dir:z-dir",
		"file:A.txt",
		"file:z.txt",
		"file:空.txt",
	}, names)

	first, err := manager.ListFileLinksPage(t.Context(), FileLinkPageRequest{
		Path: "/page", Limit: 1,
	})
	require.NoError(t, err)
	require.NotNil(t, first.NextCursor)
	require.NoError(t, manager.RemoveFileLink(t.Context(), "/page"))
	require.NoError(t, manager.CreateFileLink(t.Context(), "/page", 0, 0, true))
	_, err = manager.ListFileLinksPage(t.Context(), FileLinkPageRequest{
		Path: "/page", Cursor: first.NextCursor, Limit: 1,
	})
	require.ErrorIs(t, err, ErrFileLinkCursorStale)
}

func TestListFileLinksPageRejectsInvalidRequestsAndCorruptReferences(t *testing.T) {
	manager, _, databaseClient := newCreateFileTestManager(t, 4)
	_, err := manager.ListFileLinksPage(t.Context(), FileLinkPageRequest{Path: "/", Limit: 0})
	require.ErrorIs(t, err, ErrInvalidFileLinkPage)

	require.NoError(t, manager.CreateFileLink(t.Context(), "/corrupt", 0, 0, true))
	parent, err := manager.StatFileLink(t.Context(), "/corrupt")
	require.NoError(t, err)
	_, err = databaseClient.ExecContext(t.Context(), `
INSERT INTO tg_file_mapping_tab(
    entry_id, parent_entry_id, ref_data, file_kind, ctime, mtime,
    file_size, file_mode, file_name
) VALUES (?, ?, ?, 2, 1, 1, 1, 420, ?)`,
		parent.EntryID+1000,
		parent.EntryID,
		"not-a-file-id",
		"bad.bin",
	)
	require.NoError(t, err)
	_, err = manager.ListFileLinksPage(t.Context(), FileLinkPageRequest{
		Path: "/corrupt", Limit: 10,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "parse paged file link id")
}
