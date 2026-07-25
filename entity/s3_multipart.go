package entity

// S3FileSegment is one ordered source-file reference in a layout-v2 file.
type S3FileSegment struct {
	FileID       uint64
	SegmentIndex int
	SourceFileID uint64
	SegmentSize  int64
	Ctime        int64
	Mtime        int64
}

// S3MultipartUpload contains the durable control state for one upload ID.
type S3MultipartUpload struct {
	UploadID              string
	BucketName            string
	ObjectKey             string
	UploadState           string
	ContentType           string
	CacheControl          string
	ContentDisposition    string
	ContentEncoding       string
	ContentLanguage       string
	Expires               string
	UserMetadata          string
	ChecksumAlgorithm     string
	ChecksumType          string
	CompletionFingerprint string
	ResultFileID          uint64
	ResultETag            string
	ResultChecksumValue   string
	InitiatedAt           int64
	ExpiresAt             int64
	CompletedAt           int64
	CleanupAt             int64
	Ctime                 int64
	Mtime                 int64
}

// S3MultipartPart contains one currently registered S3 upload part.
type S3MultipartPart struct {
	UploadID      string
	PartNumber    int
	PartState     string
	FileID        uint64
	PartSize      int64
	PartETag      string
	ChecksumValue string
	UploadedAt    int64
	Ctime         int64
	Mtime         int64
}

// S3CompletedPart is one immutable final S3 multipart part. SourceFileID and
// StartOffset are derived from the composite segment manifest.
type S3CompletedPart struct {
	FileID            uint64
	PartNumber        int
	PartSize          int64
	StartOffset       int64
	PartsCount        int
	SourceFileID      uint64
	IsMultipart       bool
	ChecksumState     string
	ChecksumAlgorithm string
	ChecksumValue     string
}
