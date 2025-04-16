package directory

type directoryEntryTab struct {
	Id_            uint64 `json:"id"`
	EntryId_       uint64 `json:"entry_id"`
	ParentEntryId_ uint64 `json:"parent_entry_id"`
	RefData_       string `json:"ref_data"`
	FileKind_      int32  `json:"file_kind"`
	Ctime_         int64  `json:"ctime"`
	Mtime_         int64  `json:"mtime"`
	FileSize_      int64  `json:"file_size"`
	FileMode_      uint32 `json:"file_mode"`
	FileName_      string `json:"file_name"`
}

func (e *directoryEntryTab) ToDirectoyEntry() IDirectoryEntry {
	return e
}

func (d *directoryEntryTab) RefData() string {
	return d.RefData_
}

func (d *directoryEntryTab) Name() string {
	return d.FileName_
}

func (d *directoryEntryTab) Ctime() int64 {
	return d.Ctime_
}

func (d *directoryEntryTab) Mtime() int64 {
	return d.Mtime_
}

func (d *directoryEntryTab) Mode() uint32 {
	return d.FileMode_
}

func (d *directoryEntryTab) Size() int64 {
	return d.FileSize_
}

func (d *directoryEntryTab) IsDir() bool {
	return d.FileKind_ == defaultFileKindDir
}
