package entity

type GetFileMappingRequest struct {
	FileName string
}

type FileMappingItem struct {
	FileName string `json:"file_name"`
	FileId   uint64 `json:"file_id"`
	Ctime    uint64 `json:"ctime"`
	Mtime    uint64 `json:"mtime"`
}

type GetFileMappingResponse struct {
	Item *FileMappingItem
}

type CreateFileMappingRequest struct {
	FileName string
	FileId   uint64
}

type CreateFileMappingResponse struct {
}
