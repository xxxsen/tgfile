package file

import "github.com/xxxsen/tgfile/filemgr"

type FileHandler struct {
	m filemgr.IFileManager
}

func NewFileHandler(m filemgr.IFileManager) *FileHandler {
	return &FileHandler{
		m: m,
	}
}
