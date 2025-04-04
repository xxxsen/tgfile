package entity

type CreateFileDraftRequest struct {
	FileSize      int64
	FilePartCount int32
}

type CreateFileDraftResponse struct {
	FileId uint64
}

type MarkFileReadyRequest struct {
	FileID uint64
}

type MarkFileReadyResponse struct {
}

type CreateFilePartRequest struct {
	FileId     uint64
	FilePartId int32
	FileKey    string //真实的, 用于换取文件信息的key
}

type CreateFilePartResponse struct {
}

type GetFileInfoRequest struct {
	FileIds []uint64
}

type FileInfoItem struct {
	Id            uint64 `json:"id"`
	FileId        uint64 `json:"file_id"`
	FileSize      int64  `json:"file_size"`
	FilePartCount int32  `json:"file_part_count"`
	Ctime         int64  `json:"ctime"`
	Mtime         int64  `json:"mtime"`
	FileState     uint32 `json:"file_state"`
}

type GetFileInfoResponse struct {
	List []*FileInfoItem
}

type DeleteFileRequest struct {
	FileId []uint64
}

type DeleteFileResponse struct {
}
