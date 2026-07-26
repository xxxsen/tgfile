package directory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/xxxsen/tgfile/db"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xxxsen/common/database"
	"github.com/xxxsen/common/idgen"
)

var (
	dbc database.IDatabase
	dav IDirectory
)

func setupDirectoryTest(t *testing.T) {
	t.Helper()
	var err error
	dbc, err = db.Open(filepath.Join(t.TempDir(), "data.db"))
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, dbc.Close())
	})
	dav, err = NewDBDirectory(dbc, "tg_file_mapping_tab", idgen.Default().NextId)
	require.NoError(t, err)
}

func TestMkdir(t *testing.T) {
	setupDirectoryTest(t)
	ctx := context.Background()
	err := dav.Mkdir(ctx, "/a/b/c/d/e/f/g")
	assert.NoError(t, err)
	ents, err := dav.List(ctx, "/")
	assert.NoError(t, err)
	assert.Equal(t, 1, len(ents))
	assert.Equal(t, "a", ents[0].Name())
	ents, err = dav.List(ctx, "/a/b/c/d/e/f/")
	assert.NoError(t, err)
	assert.Equal(t, 1, len(ents))
	assert.Equal(t, "g", ents[0].Name())
	info, err := dav.Stat(ctx, "/a/b/c/d/e/f")
	assert.NoError(t, err)
	t.Logf("info:%+v", info)
}

func TestCreateFile(t *testing.T) {
	setupDirectoryTest(t)
	ctx := context.Background()
	err := dav.Create(ctx, "/1/2.txt", 123, "aaaa")
	assert.NoError(t, err)
	err = dav.Create(ctx, "/1/3.txt", 123, "bbbb")
	assert.NoError(t, err)
	ents, err := dav.List(ctx, "/1/")
	assert.NoError(t, err)
	assert.Equal(t, 2, len(ents))
	for idx, ent := range ents {
		t.Logf("item:%d => %+v", idx, ent)
	}
}

func TestReplaceReturnsPreviousEntrySnapshot(t *testing.T) {
	setupDirectoryTest(t)
	ctx := t.Context()
	require.NoError(t, dav.Create(ctx, "/replace.txt", 3, "old"))
	transactional, ok := dav.(ITransactionalDirectory)
	require.True(t, ok)
	var previous IDirectoryEntry
	err := transactional.WithTransaction(ctx, func(ctx context.Context, tx ITransaction) error {
		var err error
		previous, err = tx.Replace(ctx, "/replace.txt", 7, "new", 1234)
		return err
	})
	require.NoError(t, err)

	assert.Equal(t, "old", previous.RefData())
	assert.EqualValues(t, 3, previous.Size())
	current, err := dav.Stat(ctx, "/replace.txt")
	require.NoError(t, err)
	assert.Equal(t, "new", current.RefData())
	assert.EqualValues(t, 7, current.Size())
}

func TestStatMissingDoesNotCreateParentDirectories(t *testing.T) {
	setupDirectoryTest(t)
	ctx := context.Background()
	_, err := dav.Stat(ctx, "/stat_read_only/nested/missing.pdf")
	assert.ErrorIs(t, err, os.ErrNotExist)

	_, err = dav.Stat(ctx, "/stat_read_only")
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestListDir(t *testing.T) {
	setupDirectoryTest(t)
	ctx := context.Background()
	for i := 0; i < 1000; i++ {
		err := dav.Create(ctx, fmt.Sprintf("/list/%d.txt", i), 100, "aaaa")
		assert.NoError(t, err)
	}
	files, err := dav.List(ctx, "/list/")
	assert.NoError(t, err)
	assert.Equal(t, 1000, len(files))
}

func TestListPageUsesStableBoundedKeysetOrder(t *testing.T) {
	setupDirectoryTest(t)
	ctx := t.Context()
	for _, name := range []string{"z-dir", "A-dir", "目录"} {
		require.NoError(t, dav.Mkdir(ctx, "/paged/"+name))
	}
	for _, name := range []string{"z.txt", "A.txt", "é.txt", "空.txt"} {
		require.NoError(t, dav.Create(ctx, "/paged/"+name, 1, "1"))
	}

	var (
		cursor    *PageCursor
		collected []string
		parentID  uint64
	)
	for {
		page, err := dav.ListPage(ctx, "/paged", cursor, 2)
		require.NoError(t, err)
		require.LessOrEqual(t, len(page.Entries), 2)
		if parentID == 0 {
			parentID = page.ParentEntryID
		}
		require.Equal(t, parentID, page.ParentEntryID)
		for _, entry := range page.Entries {
			kind := "file:"
			if entry.IsDir() {
				kind = "dir:"
			}
			collected = append(collected, kind+entry.Name())
		}
		if !page.HasMore {
			break
		}
		last := page.Entries[len(page.Entries)-1]
		cursor = &PageCursor{
			IsDir:   last.IsDir(),
			Name:    last.Name(),
			EntryID: last.EntryID(),
		}
	}
	require.Equal(t, []string{
		"dir:A-dir",
		"dir:z-dir",
		"dir:目录",
		"file:A.txt",
		"file:z.txt",
		"file:é.txt",
		"file:空.txt",
	}, collected)

	_, err := dav.ListPage(ctx, "/paged", nil, 0)
	require.ErrorIs(t, err, errInvalidPageLimit)
	_, err = dav.ListPage(ctx, "/paged", &PageCursor{Name: "x"}, 1)
	require.ErrorIs(t, err, errInvalidPageCursor)
}

func TestListPageBoundaries(t *testing.T) {
	setupDirectoryTest(t)
	require.NoError(t, dav.Mkdir(t.Context(), "/empty-page"))
	empty, err := dav.ListPage(t.Context(), "/empty-page", nil, 500)
	require.NoError(t, err)
	require.Empty(t, empty.Entries)
	require.False(t, empty.HasMore)

	for index := range 501 {
		require.NoError(t, dav.Create(
			t.Context(),
			fmt.Sprintf("/boundary/%03d.bin", index),
			0,
			"1",
		))
	}
	first, err := dav.ListPage(t.Context(), "/boundary", nil, 500)
	require.NoError(t, err)
	require.Len(t, first.Entries, 500)
	require.True(t, first.HasMore)
	last := first.Entries[len(first.Entries)-1]
	second, err := dav.ListPage(t.Context(), "/boundary", &PageCursor{
		IsDir: last.IsDir(), Name: last.Name(), EntryID: last.EntryID(),
	}, 500)
	require.NoError(t, err)
	require.Len(t, second.Entries, 1)
	require.False(t, second.HasMore)
	require.Equal(t, "500.bin", second.Entries[0].Name())
}

func TestRemove(t *testing.T) {
	setupDirectoryTest(t)
	ctx := context.Background()
	testPath := "/delete_test"
	err := dav.Mkdir(ctx, testPath)
	assert.NoError(t, err)
	for i := 0; i < 10; i++ {
		for j := 0; j < 10; j++ {
			for k := 0; k < 10; k++ {
				p := fmt.Sprintf("%s/%d/%d", testPath, i, j)
				err = dav.Mkdir(ctx, p)
				assert.NoError(t, err)
				err = dav.Create(ctx, fmt.Sprintf("%s/%d.txt", p, k), 0, "")
				assert.NoError(t, err)
			}
			items, err := dav.List(ctx, fmt.Sprintf("%s/%d/%d", testPath, i, j))
			assert.NoError(t, err)
			assert.Equal(t, 10, len(items))
		}
		items, err := dav.List(ctx, fmt.Sprintf("%s/%d", testPath, i))
		assert.NoError(t, err)
		assert.Equal(t, 10, len(items))
	}
	items, err := dav.List(ctx, testPath)
	assert.NoError(t, err)
	assert.Equal(t, 10, len(items))

	// remove then
	err = dav.Remove(ctx, testPath)
	assert.NoError(t, err)
	_, err = dav.List(ctx, testPath)
	assert.Error(t, err)
}

func TestRemoveRoot(t *testing.T) {
	setupDirectoryTest(t)
	ctx := context.Background()
	err := dav.Remove(ctx, "/")
	assert.NoError(t, err)
	for i := 0; i < 10; i++ {
		err := dav.Create(ctx, fmt.Sprintf("/file_%d.txt", i), 0, "")
		assert.NoError(t, err)
	}
	for i := 0; i < 10; i++ {
		err := dav.Mkdir(ctx, fmt.Sprintf("/dir_%d", i))
		assert.NoError(t, err)
	}
	ents, err := dav.List(ctx, "/")
	assert.NoError(t, err)
	assert.Len(t, ents, 20)
	//
	err = dav.Remove(ctx, "/")
	assert.NoError(t, err)
	//
	_, err = dav.List(ctx, "/")
	assert.Error(t, err)
	//
	err = dav.Mkdir(ctx, "/")
	assert.NoError(t, err)
	ents, err = dav.List(ctx, "/")
	assert.NoError(t, err)
	assert.Len(t, ents, 0)
}

type testMovePrepareItem struct {
	link  string
	isDir bool
}

type testMoveMoveItem struct {
	src       string
	dst       string
	overwrite bool
	hasErr    bool
}

type testMoveTestItem struct {
	link  string
	exist bool
	isDir bool
}

type testMovePair struct {
	name        string
	prepareList []testMovePrepareItem
	move        testMoveMoveItem
	testList    []testMoveTestItem
}

func TestMove(t *testing.T) {
	setupDirectoryTest(t)
	ctx := context.Background()
	testPath := "/move_test"
	err := dav.Mkdir(ctx, testPath)
	assert.NoError(t, err)
	testList := []testMovePair{
		{
			name: "check_same_path",
			prepareList: []testMovePrepareItem{
				{
					link:  "/a/b/c/d",
					isDir: true,
				},
			},
			move: testMoveMoveItem{
				src:    "/a/b/c/d",
				dst:    "/a/b/c/d",
				hasErr: false,
			},
		},
		{
			name: "test_same_root",
			prepareList: []testMovePrepareItem{
				{
					link: "/a/1.txt",
				},
			},
			move: testMoveMoveItem{
				src: "/a/1.txt",
				dst: "/a/2.txt",
			},
			testList: []testMoveTestItem{
				{
					link:  "/a/1.txt",
					exist: false,
				},
				{
					link:  "/a/2.txt",
					exist: true,
				},
			},
		},
		{
			name: "test_move_on_root",
			prepareList: []testMovePrepareItem{
				{
					link: "/1.txt",
				},
			},
			move: testMoveMoveItem{
				src: "/1.txt",
				dst: "/2.txt",
			},
			testList: []testMoveTestItem{
				{
					link:  "/1.txt",
					exist: false,
				},
				{
					link:  "/2.txt",
					exist: true,
				},
			},
		},
		{
			name: "check_succ_move",
			prepareList: []testMovePrepareItem{
				{
					link:  "/a/b/c/d",
					isDir: true,
				},
				{
					link:  "/b/c",
					isDir: true,
				},
			},
			move: testMoveMoveItem{
				src: "/a/b/c/d",
				dst: "/b/c/d",
			},
			testList: []testMoveTestItem{
				{
					link:  "/a/b/c/d",
					exist: false,
				},
				{
					link:  "/a/b/c",
					exist: true,
					isDir: true,
				},
				{
					link:  "/b/c",
					exist: true,
					isDir: true,
				},
				{
					link:  "/b/c/d",
					exist: true,
					isDir: true,
				},
			},
		},
		{
			name: "check_file_move",
			prepareList: []testMovePrepareItem{
				{
					link:  "/a/1.txt",
					isDir: false,
				},
				{
					link:  "/b",
					isDir: true,
				},
			},
			move: testMoveMoveItem{
				src:       "/a/1.txt",
				dst:       "/b/2.txt",
				overwrite: false,
				hasErr:    false,
			},
			testList: []testMoveTestItem{
				{
					link:  "/a",
					exist: true,
					isDir: true,
				},
				{
					link:  "/a/1.txt",
					exist: false,
				},
				{
					link:  "/b/2.txt",
					exist: true,
				},
			},
		},
		{
			name: "check_file_overwrite",
			prepareList: []testMovePrepareItem{
				{
					link:  "/a/1.txt",
					isDir: false,
				},
				{
					link:  "/b",
					isDir: true,
				},
			},
			move: testMoveMoveItem{
				src:       "/a/1.txt",
				dst:       "/b/1.txt",
				overwrite: false,
				hasErr:    false,
			},
			testList: []testMoveTestItem{
				{
					link:  "/a",
					exist: true,
					isDir: true,
				},
				{
					link:  "/a/1.txt",
					exist: false,
				},
				{
					link:  "/b/1.txt",
					exist: true,
				},
			},
		},
		{
			name: "check_sub_path_move",
			prepareList: []testMovePrepareItem{
				{
					link:  "/a/b/c",
					isDir: true,
				},
				{
					link:  "/a/b/c/d/e",
					isDir: true,
				},
			},
			move: testMoveMoveItem{
				src:       "/a/b/c",
				dst:       "/a/b/c/d/e/c",
				overwrite: false,
				hasErr:    true,
			},
		},
		{
			name: "check_dir_overwrite_file", // 目标为文件, 那么可以overwrite
			prepareList: []testMovePrepareItem{
				{
					link:  "/a/b/c",
					isDir: true,
				},
				{
					link:  "/x/y/z/c",
					isDir: false,
				},
			},
			move: testMoveMoveItem{
				src:       "/a/b/c",
				dst:       "/x/y/z/c",
				overwrite: true,
				hasErr:    false,
			},
			testList: []testMoveTestItem{
				{
					link:  "/a/b/c",
					exist: false,
					isDir: false,
				},
				{
					link:  "/x/y/z/c",
					exist: true,
					isDir: true,
				},
			},
		},
		{
			name: "check_dir_overwrite_dir", // 目标为dir, 无法进行overwrite
			prepareList: []testMovePrepareItem{
				{
					link:  "/a/b/c",
					isDir: true,
				},
				{
					link:  "1/2/c",
					isDir: true,
				},
			},
			move: testMoveMoveItem{
				src:       "/a/b/c",
				dst:       "/1/2/c",
				overwrite: true,
				hasErr:    true,
			},
		},
	}
	for _, item := range testList {
		t.Logf("start test item, name:%s", item.name)
		_ = dav.Remove(ctx, testPath)
		for _, item := range item.prepareList {
			link := fmt.Sprintf("%s%s", testPath, item.link)
			if item.isDir {
				err = dav.Mkdir(ctx, link)
				assert.NoError(t, err)
				continue
			}
			err = dav.Create(ctx, link, 0, "")
			assert.NoError(t, err)
			ent, err := dav.Stat(ctx, link)
			assert.NoError(t, err)
			assert.Equal(t, item.isDir, ent.IsDir())
		}
		err := dav.Move(ctx, fmt.Sprintf("%s%s", testPath, item.move.src), fmt.Sprintf("%s%s", testPath, item.move.dst), item.move.overwrite)
		assert.Equal(t, item.move.hasErr, err != nil)
		if err != nil {
			t.Logf("-- get err:%v", err)
			continue
		}
		for _, tt := range item.testList {
			link := fmt.Sprintf("%s%s", testPath, tt.link)
			ent, err := dav.Stat(ctx, link)
			assert.Equal(t, tt.exist, err == nil)
			if !tt.exist {
				continue
			}
			assert.Equal(t, tt.isDir, ent.IsDir())
		}
	}
}

type testCopyPair struct {
	name        string
	prepareList []testMovePrepareItem
	copy        testMoveMoveItem
	testList    []testMoveTestItem
}

func TestCopy(t *testing.T) {
	setupDirectoryTest(t)
	ctx := context.Background()
	testPath := "/copy_test"
	err := dav.Mkdir(ctx, testPath)
	assert.NoError(t, err)
	testList := []testCopyPair{
		{
			name: "test_single_dir_copy",
			prepareList: []testMovePrepareItem{
				{
					link:  "/a/b/c",
					isDir: true,
				},
				{
					link:  "/x/y/z",
					isDir: true,
				},
			},
			copy: testMoveMoveItem{
				src:       "/a/b/c",
				dst:       "/x/y/z/c",
				overwrite: false,
				hasErr:    false,
			},
			testList: []testMoveTestItem{
				{
					link:  "/a/b/c",
					exist: true,
					isDir: true,
				},
				{
					link:  "/x/y/z/c",
					exist: true,
					isDir: true,
				},
			},
		},
		{
			name: "test_same_root",
			prepareList: []testMovePrepareItem{
				{
					link: "/a/1.txt",
				},
			},
			copy: testMoveMoveItem{
				src: "/a/1.txt",
				dst: "/a/2.txt",
			},
			testList: []testMoveTestItem{
				{
					link:  "/a/1.txt",
					exist: true,
				},
				{
					link:  "/a/2.txt",
					exist: true,
				},
			},
		},
		{
			name: "test_copy_on_root",
			prepareList: []testMovePrepareItem{
				{
					link: "/1.txt",
				},
			},
			copy: testMoveMoveItem{
				src: "/1.txt",
				dst: "/2.txt",
			},
			testList: []testMoveTestItem{
				{
					link:  "/1.txt",
					exist: true,
				},
				{
					link:  "/2.txt",
					exist: true,
				},
			},
		},
		{
			name: "test_dst_dir_with_overwrite",
			prepareList: []testMovePrepareItem{
				{
					link:  "/a/b/c",
					isDir: true,
				},
				{
					link:  "/x/y/c",
					isDir: true,
				},
			},
			copy: testMoveMoveItem{
				src:       "/a/b/c",
				dst:       "/x/y/c",
				overwrite: true,
				hasErr:    false,
			},
			testList: []testMoveTestItem{
				{link: "/a/b/c", exist: true, isDir: true},
				{link: "/x/y/c", exist: true, isDir: true},
			},
		},
		{
			name: "test_single_file_copy_no_overwrite",
			prepareList: []testMovePrepareItem{
				{
					link: "/a/b/c.txt",
				},
				{
					link: "/1/2/c.txt",
				},
			},
			copy: testMoveMoveItem{
				src:       "/a/b/c.txt",
				dst:       "/1/2/c.txt",
				overwrite: false,
				hasErr:    true,
			},
		},
		{
			name: "test_single_file_copy_overwrite",
			prepareList: []testMovePrepareItem{
				{
					link: "/a/b/1.txt",
				},
				{
					link: "/t/y/1.txt",
				},
			},
			copy: testMoveMoveItem{
				src:       "/a/b/1.txt",
				dst:       "/t/y/1.txt",
				overwrite: true,
				hasErr:    false,
			},
			testList: []testMoveTestItem{
				{
					link:  "/a/b/1.txt",
					exist: true,
					isDir: false,
				},
				{
					link:  "/t/y/1.txt",
					exist: true,
					isDir: false,
				},
			},
		},
		{
			name: "test_same_path",
			prepareList: []testMovePrepareItem{
				{
					link:  "/a/b/c",
					isDir: true,
				},
			},
			copy: testMoveMoveItem{
				src:       "/a/b/c",
				dst:       "/a/b/c",
				overwrite: true,
				hasErr:    false,
			},
		},
		{
			name: "test_sub_path",
			prepareList: []testMovePrepareItem{
				{
					link:  "/a/b/c",
					isDir: true,
				},
			},
			copy: testMoveMoveItem{
				src:       "/a/b",
				dst:       "/a/b/c/b",
				overwrite: false,
				hasErr:    true,
			},
		},
		{
			name: "test_do_recursion_copy",
			prepareList: []testMovePrepareItem{
				{
					link: "/a/b/c1/d1/1.txt",
				},
				{
					link: "/a/b/c2/d2/2.txt",
				},
				{
					link: "/a/b/c3/d3/3_1.txt",
				},
				{
					link: "/a/b/c3/d3/3_2.txt",
				},
				{
					link:  "/x/y/z",
					isDir: true,
				},
			},
			copy: testMoveMoveItem{
				src:       "/a/b",
				dst:       "/x/y/z/b",
				overwrite: false,
				hasErr:    false,
			},
			testList: []testMoveTestItem{
				{
					link:  "/a/b/c1/d1/1.txt",
					exist: true,
					isDir: false,
				},
				{
					link:  "/a/b/c2/d2/2.txt",
					exist: true,
				},
				{
					link:  "/x/y/z/b/",
					exist: true,
					isDir: true,
				},
				{
					link:  "/x/y/z/b/c1",
					exist: true,
					isDir: true,
				},
				{
					link:  "/x/y/z/b/c2",
					exist: true,
					isDir: true,
				},
				{
					link:  "/x/y/z/b/c3",
					exist: true,
					isDir: true,
				},
				{
					link:  "/x/y/z/b/c1/d1/1.txt",
					exist: true,
				},
				{
					link:  "/x/y/z/b/c2/d2/2.txt",
					exist: true,
				},
				{
					link:  "/x/y/z/b/c3/d3/3_1.txt",
					exist: true,
				},
				{
					link:  "/x/y/z/b/c3/d3/3_2.txt",
					exist: true,
				},
			},
		},
	}
	for _, item := range testList {
		t.Logf("start test copy item, name:%s", item.name)
		_ = dav.Remove(ctx, testPath)
		for _, item := range item.prepareList {
			link := fmt.Sprintf("%s%s", testPath, item.link)
			if item.isDir {
				err = dav.Mkdir(ctx, link)
				assert.NoError(t, err)
				continue
			}
			err = dav.Create(ctx, link, 0, "")
			assert.NoError(t, err)
			ent, err := dav.Stat(ctx, link)
			assert.NoError(t, err)
			assert.Equal(t, item.isDir, ent.IsDir())
		}
		err := dav.Copy(ctx, fmt.Sprintf("%s%s", testPath, item.copy.src), fmt.Sprintf("%s%s", testPath, item.copy.dst), item.copy.overwrite)
		assert.Equal(t, item.copy.hasErr, err != nil)
		if err != nil {
			t.Logf("-- get err:%v", err)
			continue
		}
		for _, tt := range item.testList {
			link := fmt.Sprintf("%s%s", testPath, tt.link)
			ent, err := dav.Stat(ctx, link)
			assert.Equal(t, tt.exist, err == nil)
			if !tt.exist {
				continue
			}
			assert.Equal(t, tt.isDir, ent.IsDir())
		}
	}
}

func TestScan(t *testing.T) {
	setupDirectoryTest(t)
	ctx := context.Background()
	_ = dav.Remove(ctx, "/")
	for i := 0; i < 10; i++ {
		err := dav.Create(ctx, fmt.Sprintf("/%d.txt", i), 0, "123")
		assert.NoError(t, err)
	}
	err := dav.Scan(ctx, 1, func(_ context.Context, res []IDirectoryEntry) (bool, error) {
		if len(res) == 0 {
			return false, nil
		}
		t.Logf("recv item:%+v", res[0])
		return true, nil
	})
	assert.NoError(t, err)
}

func TestIterateUsesBoundedStableBatches(t *testing.T) {
	setupDirectoryTest(t)
	ctx := t.Context()
	require.NoError(t, dav.Remove(ctx, "/"))
	require.NoError(t, dav.Mkdir(ctx, "/iterate"))
	const itemCount = 301
	for index := 0; index < itemCount; index++ {
		require.NoError(t, dav.Create(
			ctx,
			fmt.Sprintf("/iterate/%03d.txt", index),
			int64(index),
			strconv.Itoa(index+1),
		))
	}

	const batchSize uint = 17
	names := make([]string, 0, itemCount)
	maxBatch := 0
	err := dav.Iterate(
		ctx,
		"/iterate",
		batchSize,
		func(_ context.Context, entries []IDirectoryEntry) (bool, error) {
			maxBatch = max(maxBatch, len(entries))
			for _, entry := range entries {
				names = append(names, entry.Name())
			}
			return true, nil
		},
	)
	require.NoError(t, err)
	require.Len(t, names, itemCount)
	require.LessOrEqual(t, maxBatch, int(batchSize))
	require.IsIncreasing(t, names)
}

func TestIterateCursorDoesNotSkipAfterConcurrentDelete(t *testing.T) {
	setupDirectoryTest(t)
	ctx := t.Context()
	require.NoError(t, dav.Remove(ctx, "/"))
	require.NoError(t, dav.Mkdir(ctx, "/cursor"))
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		require.NoError(t, dav.Create(ctx, "/cursor/"+name, 1, "1"))
	}

	names := make([]string, 0, 4)
	firstBatch := true
	err := dav.Iterate(
		ctx,
		"/cursor",
		2,
		func(_ context.Context, entries []IDirectoryEntry) (bool, error) {
			for _, entry := range entries {
				names = append(names, entry.Name())
			}
			if firstBatch {
				firstBatch = false
				require.NoError(t, dav.Remove(ctx, "/cursor/a.txt"))
				require.NoError(t, dav.Create(ctx, "/cursor/d.txt", 1, "1"))
			}
			return true, nil
		},
	)
	require.NoError(t, err)
	require.Equal(t, []string{"a.txt", "b.txt", "c.txt", "d.txt"}, names)
}
