package model

import (
	"encoding/base64"
	"encoding/json"
	"mime/multipart"
)

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

type BeginUploadRequest struct {
	FileName string `json:"file_name"`
	FileSize int64  `json:"file_size"`
}

type BeginUploadResponse struct {
	UploadKey string `json:"upload_key"`
	BlockSize int64  `json:"block_size"`
}

type PartUploadRequest struct {
	UploadKey string                `form:"upload_key" binding:"required"`
	PartData  *multipart.FileHeader `form:"part_data" binding:"required"`
	PartId    *int64                `form:"part_id" binding:"required"`
}

type PartUploadResponse struct {
}

type FinishUploadRequest struct {
	UploadKey string `json:"upload_key"`
}

type FinishUploadResponse struct {
	FileKey string `json:"file_key"`
}

type MultiPartUploadContext struct {
	FileName   string `json:"file_name"`
	CreateTime int64  `json:"create_time"`
	FileId     uint64 `json:"file_id"`
	FileSize   int64  `json:"file_size"`
	BlockSize  int64  `json:"block_size"`
}

func (c *MultiPartUploadContext) Encode() (string, error) {
	raw, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

func (c *MultiPartUploadContext) Decode(key string) error {
	raw, err := base64.StdEncoding.DecodeString(key)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, c)
}
