package webdav

type webdavEntryTab struct {
	Id            uint64 `json:"id"`
	EntryId       uint64 `json:"entry_id"`
	ParentEntryId uint64 `json:"parent_entry_id"`
	RefData       string `json:"ref_data"`
	FileKind      int32  `json:"file_kind"`
	Ctime         int64  `json:"ctime"`
	Mtime         int64  `json:"mtime"`
	FileSize      int64  `json:"file_size"`
	FileMode      uint32 `json:"file_mode"`
	FileName      string `json:"file_name"`
}
