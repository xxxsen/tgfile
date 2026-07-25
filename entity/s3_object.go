package entity

type S3ObjectMetadata struct {
	EntryID                  uint64 `json:"entry_id"`
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
