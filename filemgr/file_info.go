package filemgr

import (
	"context"
	"io/fs"
	"time"
)

type defaultFileInfo struct {
	FieldSize  int64
	FieldMtime time.Time
	FieldName  string
	FieldMode  fs.FileMode
	FieldIsDir bool
	FieldSys   interface{}
}

func (d *defaultFileInfo) Name() string {
	return d.FieldName
}

func (d *defaultFileInfo) Size() int64 {
	return d.FieldSize
}

func (d *defaultFileInfo) Mode() fs.FileMode {
	if d.FieldMode == 0 {
		return 0644
	}
	return d.FieldMode
}

func (d *defaultFileInfo) ModTime() time.Time {
	return d.FieldMtime
}

func (d *defaultFileInfo) IsDir() bool {
	return d.FieldIsDir
}

func (d *defaultFileInfo) Sys() interface{} {
	return d.FieldSys

}

type dirEntry struct {
	ctx        context.Context
	FileId     uint64
	FieldIsDir bool
	FieldName  string
	FieldMode  fs.FileMode
}

func (d *dirEntry) Name() string {
	return d.FieldName
}

func (d *dirEntry) IsDir() bool {
	return d.FieldIsDir
}

func (d *dirEntry) Type() fs.FileMode {
	if d.FieldMode == 0 {
		return 0644
	}
	return d.FieldMode
}

func (d *dirEntry) Info() (fs.FileInfo, error) {
	if d.FieldIsDir {
		return &defaultFileInfo{
			FieldSize:  0,
			FieldMtime: time.Time{},
			FieldName:  d.FieldName,
			FieldMode:  d.FieldMode,
			FieldIsDir: true,
			FieldSys:   nil,
		}, nil
	}
	info, err := Stat(d.ctx, d.FileId)
	if err != nil {
		return nil, err
	}
	return &defaultFileInfo{
		FieldSize:  info.Size(),
		FieldMtime: info.ModTime(),
		FieldName:  info.Name(),
		FieldMode:  info.Mode(),
		FieldIsDir: false,
		FieldSys:   info.Sys(),
	}, nil
}
