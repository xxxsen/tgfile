package entity

type GetFilePartInfoRequest struct {
	FileId     uint64
	FilePartId []int32
}

type FilePartInfoItem struct {
	FileId      uint64 `json:"file_id"`
	FilePartId  int32  `json:"file_part_id"`
	FileKey     string `json:"file_key"`
	Ctime       int64  `json:"ctime"`
	Mtime       int64  `json:"mtime"`
	FilePartMd5 string `json:"file_part_md5"`
}

type GetFilePartInfoResponse struct {
	List []*FilePartInfoItem
}

type DeleteFilePartRequest struct {
	FileId []uint64
}

type DeleteFilePartResponse struct {
}

type ListFilePartRequest struct {
	FileId uint64
	//Offset int32
	//Limit  int32
}

type ListFilePartResponse struct {
	List []*FilePartInfoItem
}
