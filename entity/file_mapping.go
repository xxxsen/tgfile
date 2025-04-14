package entity

type GetFileLinkMetaRequest struct {
	FileName string
}

type FileLinkMeta struct {
	FileName string `json:"file_name"`
	FileId   uint64 `json:"file_id"`
	FileSize int64  `json:"file_size"`
	Mode     uint32 `json:"mode"`
	Ctime    int64  `json:"ctime"`
	Mtime    int64  `json:"mtime"`
	IsDir    bool   `json:"is_dir"`
}

type GetFileLinkMetaResponse struct {
	Item *FileLinkMeta
}

type CreateFileLinkRequest struct {
	FileName string
	FileSize int64
	FileId   uint64
	IsDir    bool
}

type CreateFileLinkResponse struct {
}
