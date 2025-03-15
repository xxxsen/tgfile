package model

import "mime/multipart"

type DownloadFileRequest struct {
	Key string `form:"key" binding:"required"`
}

type UploadFileRequest struct {
	File *multipart.FileHeader `form:"file" binding:"required"`
}

type UploadFileResponse struct {
	Key string `json:"key"`
}

type GetFileInfoRequest struct {
	Key string `form:"key"  binding:"required"`
}

type FileInfoItem struct {
	Key      string `json:"key"`
	Exist    bool   `json:"exist"`
	FileSize int64  `json:"file_size"`
	Ctime    int64  `json:"ctime"`
	Mtime    int64  `json:"mtime"`
}

type GetFileInfoResponse struct {
	Item *FileInfoItem `json:"item"`
}
