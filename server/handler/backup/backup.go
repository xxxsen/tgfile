package backup

import "github.com/xxxsen/tgfile/filemgr"

type BackupHandler struct {
	fmgr filemgr.IFileManager
}

func NewBackupHandler(fmgr filemgr.IFileManager) *BackupHandler {
	return &BackupHandler{fmgr: fmgr}
}
