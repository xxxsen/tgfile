package directory

import (
	"context"
	"fmt"
	"os"
	"testing"
	"tgfile/db"

	"github.com/stretchr/testify/assert"
	"github.com/xxxsen/common/database"
	"github.com/xxxsen/common/idgen"
)

var (
	dbfile = "/tmp/sqlite_webdav_test.db"
	dbc    database.IDatabase
	dav    IDirectory
)

func setup() {
	tearDown()
	var err error
	if err := db.InitDB(dbfile); err != nil {
		panic(err)
	}
	dbc = db.GetClient()
	dav, err = NewDBDirectory(dbc, "tg_file_mapping_tab", idgen.Default().NextId)
	if err != nil {
		panic(err)
	}
}

func tearDown() {
	if dbc != nil {
		_ = dbc.Close()
	}
	os.RemoveAll(dbfile)
}

func TestMain(m *testing.M) {
	setup()
	code := m.Run()
	tearDown()
	if code != 0 {
		os.Exit(code)
	}
}

func TestMkdir(t *testing.T) {
	ctx := context.Background()
	err := dav.Mkdir(ctx, "/a/b/c/d/e/f/g")
	assert.NoError(t, err)
	ents, err := dav.List(ctx, "/")
	assert.NoError(t, err)
	assert.Equal(t, 1, len(ents))
	assert.Equal(t, "a", ents[0].Name)
	ents, err = dav.List(ctx, "/a/b/c/d/e/f/")
	assert.NoError(t, err)
	assert.Equal(t, 1, len(ents))
	assert.Equal(t, "g", ents[0].Name)
	info, err := dav.Stat(ctx, "/a/b/c/d/e/f")
	assert.NoError(t, err)
	t.Logf("info:%+v", *info)
}

func TestCreateFile(t *testing.T) {
	ctx := context.Background()
	err := dav.Create(ctx, "/1/2.txt", 123, "aaaa")
	assert.NoError(t, err)
	err = dav.Create(ctx, "/1/3.txt", 123, "bbbb")
	assert.NoError(t, err)
	ents, err := dav.List(ctx, "/1/")
	assert.NoError(t, err)
	assert.Equal(t, 2, len(ents))
	for idx, ent := range ents {
		t.Logf("item:%d => %+v", idx, *ent)
	}
}

func TestListDir(t *testing.T) {
	ctx := context.Background()
	for i := 0; i < 1000; i++ {
		err := dav.Create(ctx, fmt.Sprintf("/list/%d.txt", i), 100, "aaaa")
		assert.NoError(t, err)
	}
	files, err := dav.List(ctx, "/list/")
	assert.NoError(t, err)
	assert.Equal(t, 1000, len(files))
}

func TestRemove(t *testing.T) {
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

	//remove then
	err = dav.Remove(ctx, testPath)
	assert.NoError(t, err)
	_, err = dav.List(ctx, testPath)
	assert.Error(t, err)
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
				hasErr: true,
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
			name: "check_dir_overwrite_file", //目标为文件, 那么可以overwrite
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
			name: "check_dir_overwrite_dir", //目标为dir, 无法进行overwrite
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
			assert.Equal(t, item.isDir, ent.IsDir)
		}
		err := dav.Move(ctx, fmt.Sprintf("%s%s", testPath, item.move.src), fmt.Sprintf("%s%s", testPath, item.move.dst), item.move.overwrite)
		assert.Equal(t, item.move.hasErr, err != nil)
		if err != nil {
			continue
		}
		for _, tt := range item.testList {
			link := fmt.Sprintf("%s%s", testPath, tt.link)
			ent, err := dav.Stat(ctx, link)
			assert.Equal(t, tt.exist, err == nil)
			if !tt.exist {
				continue
			}
			assert.Equal(t, tt.isDir, ent.IsDir)
		}
	}

}
