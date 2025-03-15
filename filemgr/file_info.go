package filemgr

import (
	"io/fs"
	"time"
)

type defaultFileInfo struct {
	FileSize  int64
	FileMtime time.Time
	FileName  string
}

func (d *defaultFileInfo) Name() string {
	return d.FileName
}

func (d *defaultFileInfo) Size() int64 {
	return d.FileSize
}

func (d *defaultFileInfo) Mode() fs.FileMode {
	return 0644
}

func (d *defaultFileInfo) ModTime() time.Time {
	return d.FileMtime
}

func (d *defaultFileInfo) IsDir() bool {
	return false
}

func (d *defaultFileInfo) Sys() interface{} {
	return nil

}
