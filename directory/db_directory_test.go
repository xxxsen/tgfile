package directory

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/xxxsen/common/database"
	"github.com/xxxsen/common/database/sqlite"
	"github.com/xxxsen/common/idgen"
)

var (
	dbfile = "/tmp/sqlite_webdav_test.db"
	db     database.IDatabase
	dav    IDirectory
)

func setup() {
	tearDown()
	var err error
	db, err = sqlite.New(dbfile)
	if err != nil {
		panic(err)
	}
	dav, err = NewDBDirectory(db, "t_test_tab", idgen.Default().NextId)
	if err != nil {
		panic(err)
	}
}

func tearDown() {
	if db != nil {
		_ = db.Close()
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
