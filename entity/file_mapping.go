package entity

type GetFileMappingRequest struct {
	FileName string
}

type FileMappingItem struct {
	FileName string `json:"file_name"`
	FileId   uint64 `json:"file_id"`
	FileSize int64  `json:"file_size"`
	Mode     uint32 `json:"mode"`
	Ctime    int64  `json:"ctime"`
	Mtime    int64  `json:"mtime"`
	IsDir    bool   `json:"is_dir"`
}

type GetFileMappingResponse struct {
	Item *FileMappingItem
}

type CreateFileMappingRequest struct {
	FileName string
	FileSize int64
	FileId   uint64
	IsDir    bool
}

type CreateFileMappingResponse struct {
}
