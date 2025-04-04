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

func (e *directoryEntryTab) ToDirectoyEntry() IDirectoryEntry {
	return e
}

func (d *directoryEntryTab) GetRefData() string {
	return d.RefData
}

func (d *directoryEntryTab) GetName() string {
	return d.FileName
}

func (d *directoryEntryTab) GetCtime() int64 {
	return d.Ctime
}

func (d *directoryEntryTab) GetMtime() int64 {
	return d.Mtime
}

func (d *directoryEntryTab) GetMode() uint32 {
	return d.FileMode
}

func (d *directoryEntryTab) GetSize() int64 {
	return d.FileSize
}

func (d *directoryEntryTab) GetIsDir() bool {
	return d.FileKind == defaultFileKindDir
}
