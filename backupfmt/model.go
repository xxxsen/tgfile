package backupfmt

const (
	FormatName    = "tgfile-logical-backup"
	FormatVersion = 2
	MediaType     = "application/vnd.tgfile.backup.v2+tar+gzip"
	FormatEntry   = "format.json"
	ManifestEntry = "manifest.json"
)

type FormatHeader struct {
	Format  string `json:"format"`
	Version int    `json:"version"`
}

type Manifest struct {
	Format           string           `json:"format"`
	Version          int              `json:"version"`
	CreatedAt        string           `json:"created_at"`
	Scope            string           `json:"scope"`
	Source           Source           `json:"source"`
	Limits           Summary          `json:"limits"`
	RequiredBuckets  []RequiredBucket `json:"required_buckets"`
	Files            []File           `json:"files"`
	Directories      []Directory      `json:"directories"`
	Mappings         []Mapping        `json:"mappings"`
	S3Objects        []S3Object       `json:"s3_objects"`
	WebDAVProperties []WebDAVProperty `json:"webdav_properties"`
}

type Source struct {
	SchemaVersion int    `json:"schema_version"`
	BlockIOKind   string `json:"blockio_kind"`
	MaxPartSize   int64  `json:"max_part_size"`
}

type Summary struct {
	MappingCount   int64 `json:"mapping_count"`
	DirectoryCount int64 `json:"directory_count"`
	FileCount      int64 `json:"file_count"`
	PartCount      int64 `json:"part_count"`
	PhysicalBytes  int64 `json:"physical_bytes"`
}

type RequiredBucket struct {
	Name string `json:"name"`
	ACL  string `json:"acl"`
}

type File struct {
	Ref              string          `json:"ref"`
	SourceFileID     string          `json:"source_file_id"`
	LayoutVersion    int             `json:"layout_version"`
	Size             int64           `json:"size"`
	CompatibilityMD5 string          `json:"compatibility_md5"`
	Ctime            int64           `json:"ctime"`
	Mtime            int64           `json:"mtime"`
	Parts            []Part          `json:"parts"`
	Segments         []Segment       `json:"segments"`
	CompletedParts   []CompletedPart `json:"completed_parts"`
}

type Part struct {
	Index  int    `json:"index"`
	Size   int64  `json:"size"`
	MD5    string `json:"md5"`
	SHA256 string `json:"sha256"`
	Entry  string `json:"entry"`
}

type Segment struct {
	Index     int    `json:"index"`
	SourceRef string `json:"source_ref"`
	Size      int64  `json:"size"`
}

type CompletedPart struct {
	PartNumber        int    `json:"part_number"`
	PartSize          int64  `json:"part_size"`
	ChecksumState     string `json:"checksum_state"`
	ChecksumAlgorithm string `json:"checksum_algorithm"`
	ChecksumValue     string `json:"checksum_value"`
	Ctime             int64  `json:"ctime"`
	Mtime             int64  `json:"mtime"`
}

type Directory struct {
	Path  string `json:"path"`
	Mode  uint32 `json:"mode"`
	Ctime int64  `json:"ctime"`
	Mtime int64  `json:"mtime"`
}

type Mapping struct {
	Path    string `json:"path"`
	FileRef string `json:"file_ref"`
	Size    int64  `json:"size"`
	Mode    uint32 `json:"mode"`
	Ctime   int64  `json:"ctime"`
	Mtime   int64  `json:"mtime"`
}

type S3Object struct {
	Path                     string `json:"path"`
	ETag                     string `json:"etag"`
	ChecksumSHA256           string `json:"checksum_sha256"`
	RequestChecksumAlgorithm string `json:"request_checksum_algorithm"`
	RequestChecksumValue     string `json:"request_checksum_value"`
	ChecksumType             string `json:"checksum_type"`
	ContentType              string `json:"content_type"`
	CacheControl             string `json:"cache_control"`
	ContentDisposition       string `json:"content_disposition"`
	ContentEncoding          string `json:"content_encoding"`
	ContentLanguage          string `json:"content_language"`
	Expires                  string `json:"expires"`
	UserMetadata             string `json:"user_metadata"`
	Ctime                    int64  `json:"ctime"`
	Mtime                    int64  `json:"mtime"`
}

type WebDAVProperty struct {
	Path         string `json:"path"`
	NamespaceURI string `json:"namespace_uri"`
	LocalName    string `json:"local_name"`
	ValueXML     string `json:"value_xml"`
	Ctime        int64  `json:"ctime"`
	Mtime        int64  `json:"mtime"`
}

type Limits struct {
	MaxArchiveBytes  int64
	MaxExpandedBytes int64
	MaxMappingCount  int
	MaxFileCount     int
	MaxPartCount     int
	MaxPathBytes     int
	MaxManifestBytes int64
	MaxPropertyBytes int
	MaxUserMetaBytes int
}

func DefaultLimits() Limits {
	return Limits{
		MaxArchiveBytes:  100 * 1024 * 1024 * 1024,
		MaxExpandedBytes: 1024 * 1024 * 1024 * 1024,
		MaxMappingCount:  100_000,
		MaxFileCount:     100_000,
		MaxPartCount:     1_000_000,
		MaxPathBytes:     1024,
		MaxManifestBytes: 256 * 1024 * 1024,
		MaxPropertyBytes: 1024 * 1024,
		MaxUserMetaBytes: 8 * 1024,
	}
}

type Report struct {
	ArtifactSHA256 string  `json:"artifact_sha256"`
	ArtifactBytes  int64   `json:"artifact_bytes"`
	Summary        Summary `json:"summary"`
}
