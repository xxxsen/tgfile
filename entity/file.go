package entity

import "encoding/json"

// EmptyFileMD5Sum is the standard hexadecimal MD5 digest of an empty byte sequence.
const EmptyFileMD5Sum = "d41d8cd98f00b204e9800998ecf8427e"

type CreateFileDraftRequest struct {
	FileSize      int64
	FilePartCount int32
}

type CreateFileDraftResponse struct {
	FileId uint64
}

type MarkFileReadyRequest struct {
	FileID  uint64
	Extinfo string
}

type MarkFileReadyResponse struct{}

type CreateFilePartRequest struct {
	FileId      uint64
	FilePartId  int32
	FileKey     string // 真实的, 用于换取文件信息的key
	FilePartMd5 string
	BackendKind string
	DeleteRef   string
	UploadedAt  int64
}

type CreateFilePartResponse struct{}

type GetFileInfoRequest struct {
	FileIds []uint64
}

type FileInfoItem struct {
	Id                uint64 `json:"id"`
	FileId            uint64 `json:"file_id"`
	FileSize          int64  `json:"file_size"`
	FilePartCount     int32  `json:"file_part_count"`
	Ctime             int64  `json:"ctime"`
	Mtime             int64  `json:"mtime"`
	FileState         uint32 `json:"file_state"`
	Extinfo           string `json:"extinfo"`
	FileLayoutVersion int32  `json:"file_layout_version"`
}

type FileExtInfo struct {
	Md5 string `json:"md5"`
}

func (f *FileInfoItem) ToFileMeta() *FileMeta {
	fm := &FileMeta{
		FileId:        f.FileId,
		FileSize:      f.FileSize,
		Ctime:         f.Ctime,
		Mtime:         f.Mtime,
		FileState:     f.FileState,
		FilePartCount: f.FilePartCount,
		LayoutVersion: f.FileLayoutVersion,
	}
	if len(f.Extinfo) != 0 && f.Extinfo != "{}" {
		var extinfo FileExtInfo
		if err := json.Unmarshal([]byte(f.Extinfo), &extinfo); err == nil {
			fm.Md5Sum = extinfo.Md5
		}
	}
	if f.FileSize == 0 && fm.Md5Sum == "" {
		fm.Md5Sum = EmptyFileMD5Sum
	}
	return fm
}

type GetFileInfoResponse struct {
	List []*FileInfoItem
}

type DeleteFileRequest struct {
	FileId []uint64
}

type DeleteFileResponse struct{}

type FileMeta struct {
	FileId        uint64
	FileSize      int64
	Ctime         int64
	Mtime         int64
	FileState     uint32
	Md5Sum        string
	FilePartCount int32
	LayoutVersion int32
}
