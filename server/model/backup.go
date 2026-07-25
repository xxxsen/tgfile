package model

import "mime/multipart"

type ExportRequest struct{}

type ImportRequest struct {
	File *multipart.FileHeader `form:"file" binding:"required"`
}

type StatisticInfo struct {
	FileCount int64 `json:"file_count"`
	FileSize  int64 `json:"file_size"`
	TimeCost  int64 `json:"time_cost"`
}
