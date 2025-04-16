package s3

import "github.com/xxxsen/tgfile/filemgr"

type S3Handler struct {
	fmgr filemgr.IFileManager
}

func NewS3Handler(fmgr filemgr.IFileManager) *S3Handler {
	return &S3Handler{fmgr: fmgr}
}
