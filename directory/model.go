package directory

type directoryEntryTab struct {
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

func (e *directoryEntryTab) ToDirectoyEntry() *DirectoryEntry {
	rs := &DirectoryEntry{
		RefData: e.RefData,
		Name:    e.FileName,
		Ctime:   e.Ctime,
		Mtime:   e.Mtime,
		Mode:    e.FileMode,
		Size:    e.FileSize,
		IsDir:   e.FileKind == defaultFileKindDir,
	}
	return rs
}

type DirectoryEntry struct {
	RefData string
	Name    string
	Ctime   int64
	Mtime   int64
	Mode    uint32
	Size    int64
	IsDir   bool
}
