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

func (e *directoryEntryTab) RefData() string {
	return e.RefData_
}

func (e *directoryEntryTab) EntryID() uint64 {
	return e.EntryId_
}

func (e *directoryEntryTab) Name() string {
	return e.FileName_
}

func (e *directoryEntryTab) Ctime() int64 {
	return e.Ctime_
}

func (e *directoryEntryTab) Mtime() int64 {
	return e.Mtime_
}

func (e *directoryEntryTab) Mode() uint32 {
	return e.FileMode_
}

func (e *directoryEntryTab) Size() int64 {
	return e.FileSize_
}

func (e *directoryEntryTab) IsDir() bool {
	return e.FileKind_ == defaultFileKindDir
}
