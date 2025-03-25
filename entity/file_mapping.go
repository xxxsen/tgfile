package entity

import (
	"io/fs"
	"time"
)

type GetFileMappingRequest struct {
	FileName string
}

type FileMappingItem struct {
	FileName   string `json:"file_name"`
	FileId     uint64 `json:"file_id"`
	FileSize   int64  `json:"file_size"`
	IsDirEntry bool   `json:"is_dir"`
	Ctime      uint64 `json:"ctime"`
	Mtime      uint64 `json:"mtime"`
	FileMode   uint32 `json:"file_mode"`
}

type GetFileMappingResponse struct {
	Item *FileMappingItem
}

type CreateFileMappingRequest struct {
	FileName string
	FileId   uint64
	IsDir    bool
	FileMode uint32
	Ctime    uint64
	Mtime    uint64
	FileSize int64
}

type CreateLinkOption struct {
	FileMode           uint32
	IsDir              bool
	Ctime              uint64
	Mtime              uint64
	FileSize           int64
	EnsureZeroFileSize bool
}

type CreateFileMappingResponse struct {
}

func (f *FileMappingItem) Name() string {
	return f.FileName
}

func (f *FileMappingItem) Size() int64 {
	return f.FileSize
}

func (f *FileMappingItem) Mode() fs.FileMode {
	m := f.FileMode
	if m == 0 {
		m = 0755
	}
	return fs.FileMode(m)
}

func (f *FileMappingItem) ModTime() time.Time {
	return time.UnixMilli(int64(f.Mtime))
}

func (f *FileMappingItem) IsDir() bool {
	return f.IsDirEntry
}

func (f *FileMappingItem) Sys() any {
	return f
}
