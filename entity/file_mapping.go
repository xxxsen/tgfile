package entity

type GetFileMappingRequest struct {
	FileName string
}

type FileMappingItem struct {
	FileName string `json:"file_name"`
	FileId   uint64 `json:"file_id"`
	FileSize int64  `json:"file_size"`
	Ctime    int64  `json:"ctime"`
	Mtime    int64  `json:"mtime"`
}

type GetFileMappingResponse struct {
	Item *FileMappingItem
}

type CreateFileMappingRequest struct {
	FileName string
	FileSize int64
	FileId   uint64
}

type CreateFileMappingResponse struct {
}
