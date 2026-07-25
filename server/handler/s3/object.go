package s3

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xxxsen/tgfile/entity"
	"github.com/xxxsen/tgfile/filemgr"
	"github.com/xxxsen/tgfile/s3checksum"
	"github.com/xxxsen/tgfile/server/handler/s3/s3base"

	"github.com/gin-gonic/gin"
	"github.com/xxxsen/common/logutil"
	"github.com/xxxsen/mimetype"
	"go.uber.org/zap"
)

const (
	maxObjectKeyBytes         = 1024
	maxMetadataKeySize        = 128
	maxMetadataValue          = 2 * 1024
	maxMetadataTotal          = 8 * 1024
	defaultObjectCacheControl = "public, max-age=604800"
)

var (
	errContentLengthMissing            = errors.New("content length is required")
	errInvalidObjectName               = errors.New("invalid object name")
	errChecksumMismatch                = errors.New("request checksum mismatch")
	errVerifiedTrailerType             = errors.New("invalid verified trailer context")
	errInvalidObjectRange              = errors.New("invalid object range")
	errEmptyMultipartChecksumAlgorithm = errors.New("multipart checksum algorithm is empty")
)

func (h *S3Handler) DownloadObject(c *gin.Context) {
	if h.rejectUnsupportedObjectQuery(c) {
		return
	}
	bucket, key, apiError := h.authorizeObject(c)
	if apiError != nil {
		s3base.WriteError(c, apiError)
		return
	}
	if apiError := validateChecksumMode(c.Request); apiError != nil {
		s3base.WriteError(c, apiError)
		return
	}
	if err := validateHistoricalObjectKeyBoundary(bucket.Name, key); err != nil {
		s3base.WriteError(c, objectNameError(err))
		return
	}
	objectPath := "/" + bucket.Name + "/" + key
	info, err := h.fmgr.StatS3Object(c.Request.Context(), objectPath)
	if err != nil {
		s3base.WriteError(c, objectError(err, bucket.Name, key, objectPath))
		return
	}
	if apiError := checkReadConditions(c.Request, info); apiError != nil {
		s3base.WriteError(c, apiError)
		return
	}
	if apiError := validateObjectRange(c.Request, info); apiError != nil {
		apiError.Bucket = bucket.Name
		apiError.Key = key
		apiError.Resource = objectPath
		s3base.WriteError(c, apiError)
		return
	}
	file, err := h.fmgr.OpenFile(c.Request.Context(), info.Link.FileId)
	if err != nil {
		s3base.WriteError(c, s3base.InternalError(fmt.Errorf("open S3 object: %w", err)))
		return
	}
	defer logCloseError(c.Request.Context(), file, "close S3 download")
	includeChecksum := c.GetHeader("Range") == "" || !objectRangeApplies(c.Request, info)
	setObjectHeaders(c, info, includeChecksum)
	http.ServeContent(
		c.Writer,
		c.Request,
		path.Base(key),
		time.UnixMilli(info.Link.Mtime),
		file,
	)
}

func (h *S3Handler) HeadObject(c *gin.Context) {
	if h.rejectUnsupportedObjectQuery(c) {
		return
	}
	bucket, key, apiError := h.authorizeObject(c)
	if apiError != nil {
		s3base.WriteError(c, apiError)
		return
	}
	if apiError := validateChecksumMode(c.Request); apiError != nil {
		s3base.WriteError(c, apiError)
		return
	}
	if err := validateHistoricalObjectKeyBoundary(bucket.Name, key); err != nil {
		s3base.WriteError(c, objectNameError(err))
		return
	}
	objectPath := "/" + bucket.Name + "/" + key
	info, err := h.fmgr.StatS3Object(c.Request.Context(), objectPath)
	if err != nil {
		s3base.WriteError(c, objectError(err, bucket.Name, key, objectPath))
		return
	}
	if apiError := checkReadConditions(c.Request, info); apiError != nil {
		s3base.WriteError(c, apiError)
		return
	}
	setObjectHeaders(c, info, true)
	c.Header("Accept-Ranges", "bytes")
	c.Header("Content-Length", strconv.FormatInt(info.Link.FileSize, 10))
	c.Status(http.StatusOK)
}

func (h *S3Handler) UploadObject(c *gin.Context) {
	query := c.Request.URL.Query()
	hasPartNumber := hasQueryKey(query, "partNumber")
	hasUploadID := hasQueryKey(query, "uploadId")
	if hasPartNumber || hasUploadID {
		if !hasPartNumber || !hasUploadID {
			if _, apiError := h.Authorize(c, true); apiError != nil {
				s3base.WriteError(c, apiError)
				return
			}
			s3base.WriteError(c, invalidMultipartArgument("partNumber and uploadId must be provided together.", nil))
			return
		}
		h.UploadPart(c)
		return
	}
	if h.rejectUnsupportedObjectQuery(c) {
		return
	}
	if c.GetHeader("x-amz-copy-source") != "" {
		h.CopyObject(c)
		return
	}
	preparation, apiError := h.prepareUpload(c)
	if apiError != nil {
		s3base.WriteError(c, apiError)
		return
	}
	unlock := h.locks.lock(preparation.objectPath)
	defer unlock()
	if apiError := h.fastCheckCondition(
		c.Request.Context(),
		preparation.objectPath,
		preparation.condition,
	); apiError != nil {
		s3base.WriteError(c, apiError)
		return
	}
	fileID, hashes, apiError := h.receiveUpload(c, preparation.size)
	if apiError != nil {
		s3base.WriteError(c, apiError)
		return
	}
	preparation.metadata.ETag = `"` + hex.EncodeToString(hashes.md5.Sum(nil)) + `"`
	preparation.metadata.ChecksumSHA256 = base64.StdEncoding.EncodeToString(hashes.sha256.Sum(nil))
	if hashes.request != nil {
		preparation.metadata.RequestChecksumAlgorithm = string(hashes.algorithm)
		preparation.metadata.RequestChecksumValue = hashes.expected
		preparation.metadata.ChecksumType = "FULL_OBJECT"
	}
	info, err := h.fmgr.PublishS3Object(
		c.Request.Context(),
		preparation.objectPath,
		fileID,
		preparation.size,
		preparation.metadata,
		preparation.condition,
	)
	if err != nil {
		discardUploadedFile(c.Request.Context(), h.fmgr, fileID)
		s3base.WriteError(c, mutationError(err))
		return
	}
	c.Header("ETag", info.Metadata.ETag)
	c.Header("x-amz-checksum-sha256", info.Metadata.ChecksumSHA256)
	if info.Metadata.RequestChecksumAlgorithm != "" {
		c.Header(
			checksumHeader(info.Metadata.RequestChecksumAlgorithm),
			info.Metadata.RequestChecksumValue,
		)
	}
	c.Status(http.StatusOK)
}

func (h *S3Handler) rejectUnsupportedObjectQuery(c *gin.Context) bool {
	if !hasUnsupportedObjectQuery(c.Request.URL.Query()) {
		return false
	}
	if _, apiError := h.Authorize(c, true); apiError != nil {
		s3base.WriteError(c, apiError)
		return true
	}
	s3base.WriteError(c, s3base.NewError(
		http.StatusNotImplemented,
		"NotImplemented",
		"The requested object subresource is not implemented.",
		nil,
	))
	return true
}

func hasUnsupportedObjectQuery(query url.Values) bool {
	for key := range query {
		lower := strings.ToLower(key)
		if strings.HasPrefix(lower, "response-") {
			return true
		}
		switch lower {
		case "accelerate", "acl", "analytics", "attributes", "cors", "delete",
			"encryption", "inventory", "legal-hold", "lifecycle", "location",
			"logging", "metrics", "notification", "object-lock", "ownershipcontrols",
			"partnumber", "policy", "publicaccessblock", "replication", "requestpayment",
			"restore", "retention", "select", "select-type", "tagging", "torrent",
			"uploadid", "uploads", "versionid", "versioning", "website":
			return true
		}
	}
	return false
}

type uploadPreparation struct {
	objectPath string
	size       int64
	metadata   *entity.S3ObjectMetadata
	condition  *filemgr.S3Condition
}

func (h *S3Handler) prepareUpload(c *gin.Context) (*uploadPreparation, *s3base.APIError) {
	bucket, key, apiError := h.authorizeWrite(c)
	if apiError != nil {
		return nil, apiError
	}
	if apiError := rejectObjectACLHeaders(c.Request); apiError != nil {
		return nil, apiError
	}
	if err := validateNewObjectKey(key); err != nil {
		return nil, objectNameError(err)
	}
	if apiError := validateAWSChunkedUpload(c); apiError != nil {
		return nil, apiError
	}
	size := uploadSize(c)
	if size < 0 {
		return nil, s3base.NewError(
			http.StatusLengthRequired,
			"MissingContentLength",
			"You must provide the Content-Length HTTP header.",
			errContentLengthMissing,
		)
	}
	if h.maxObjectSize > 0 && size > h.maxObjectSize {
		return nil, s3base.NewError(
			http.StatusBadRequest,
			"EntityTooLarge",
			"Your proposed upload exceeds the configured maximum object size.",
			nil,
		)
	}
	metadata, apiError := parseRequestMetadata(c.Request, key)
	if apiError != nil {
		return nil, apiError
	}
	condition, apiError := parseDestinationCondition(c.Request)
	if apiError != nil {
		return nil, apiError
	}
	return &uploadPreparation{
		objectPath: "/" + bucket.Name + "/" + key,
		size:       size,
		metadata:   metadata,
		condition:  condition,
	}, nil
}

func validateAWSChunkedUpload(c *gin.Context) *s3base.APIError {
	if !containsContentEncoding(c.GetHeader("Content-Encoding"), "aws-chunked") {
		return nil
	}
	if _, exists := c.Get("s3-decoded-content-length"); !exists {
		return s3base.InvalidRequest(
			"aws-chunked content encoding requires a verified streaming SigV4 payload.",
			nil,
		)
	}
	return nil
}

func uploadSize(c *gin.Context) int64 {
	if decodedLength, exists := c.Get("s3-decoded-content-length"); exists {
		if value, ok := decodedLength.(int64); ok {
			return value
		}
	}
	return c.Request.ContentLength
}

func (h *S3Handler) receiveUpload(
	c *gin.Context,
	size int64,
) (uint64, *uploadHashes, *s3base.APIError) {
	hashes, reader, apiError := newUploadHashes(c.Request)
	if apiError != nil {
		return 0, nil, apiError
	}
	fileID, err := h.fmgr.CreateFile(c.Request.Context(), size, reader)
	if err != nil {
		return 0, nil, uploadError(err)
	}
	if _, err := io.Copy(io.Discard, reader); err != nil {
		discardUploadedFile(c.Request.Context(), h.fmgr, fileID)
		return 0, nil, uploadError(err)
	}
	if apiError := hashes.loadTrailer(c); apiError != nil {
		discardUploadedFile(c.Request.Context(), h.fmgr, fileID)
		return 0, nil, apiError
	}
	if apiError := hashes.validate(); apiError != nil {
		discardUploadedFile(c.Request.Context(), h.fmgr, fileID)
		return 0, nil, apiError
	}
	return fileID, hashes, nil
}

func (h *S3Handler) receiveMultipartUpload(
	c *gin.Context,
	size int64,
	spec *filemgr.MultipartChecksumSpec,
) (uint64, *uploadHashes, *s3base.APIError) {
	hashes, reader, apiError := newMultipartUploadHashes(c.Request, spec)
	if apiError != nil {
		return 0, nil, apiError
	}
	fileID, err := h.fmgr.CreateFile(c.Request.Context(), size, reader)
	if err != nil {
		return 0, nil, uploadError(err)
	}
	if _, err := io.Copy(io.Discard, reader); err != nil {
		discardUploadedFile(c.Request.Context(), h.fmgr, fileID)
		return 0, nil, uploadError(err)
	}
	if apiError := hashes.loadTrailer(c); apiError != nil {
		discardUploadedFile(c.Request.Context(), h.fmgr, fileID)
		return 0, nil, apiError
	}
	if apiError := hashes.validate(); apiError != nil {
		discardUploadedFile(c.Request.Context(), h.fmgr, fileID)
		return 0, nil, apiError
	}
	return fileID, hashes, nil
}

func discardUploadedFile(ctx context.Context, manager filemgr.IFileManager, fileID uint64) {
	cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if err := manager.DiscardUnpublishedFile(cleanupContext, fileID); err != nil {
		logutil.GetLogger(ctx).Error(
			"discard unpublished uploaded file failed",
			zap.Error(err),
			zap.Uint64("file_id", fileID),
		)
	}
}

func (h *S3Handler) authorizeObject(c *gin.Context) (Bucket, string, *s3base.APIError) {
	bucketName, key := requestBucketKey(c.Request.URL.Path)
	bucket, exists := h.Bucket(bucketName)
	if !exists {
		return Bucket{}, "", s3base.NewError(
			http.StatusNotFound,
			"NoSuchBucket",
			"The specified bucket does not exist.",
			nil,
		)
	}
	required := bucket.ACL != BucketACLPublicRead
	if _, apiError := h.Authorize(c, required); apiError != nil {
		return Bucket{}, "", apiError
	}
	return bucket, key, nil
}

func (h *S3Handler) authorizeWrite(c *gin.Context) (Bucket, string, *s3base.APIError) {
	bucketName, key := requestBucketKey(c.Request.URL.Path)
	bucket, exists := h.Bucket(bucketName)
	if !exists {
		return Bucket{}, "", s3base.NewError(
			http.StatusNotFound,
			"NoSuchBucket",
			"The specified bucket does not exist.",
			nil,
		)
	}
	if _, apiError := h.Authorize(c, true); apiError != nil {
		return Bucket{}, "", apiError
	}
	return bucket, key, nil
}

func requestBucketKey(requestPath string) (string, string) {
	trimmed := strings.TrimPrefix(requestPath, "/")
	bucket, key, _ := strings.Cut(trimmed, "/")
	return bucket, key
}

func validateNewObjectKey(key string) error {
	if !validObjectKeyShape(key) {
		return errInvalidObjectName
	}
	for _, character := range key {
		if character == 0 || character < 0x20 || character == 0x7f {
			return errInvalidObjectName
		}
	}
	for _, segment := range strings.Split(key, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return errInvalidObjectName
		}
	}
	if strings.TrimPrefix(path.Clean("/"+key), "/") != key {
		return errInvalidObjectName
	}
	return nil
}

func validObjectKeyShape(key string) bool {
	return key != "" &&
		len(key) <= maxObjectKeyBytes &&
		utf8.ValidString(key) &&
		!strings.HasPrefix(key, "/") &&
		!strings.HasSuffix(key, "/") &&
		!strings.ContainsRune(key, '\\')
}

func validateHistoricalObjectKeyBoundary(bucket, key string) error {
	if key == "" || strings.HasPrefix(key, "/") || strings.HasSuffix(key, "/") {
		return errInvalidObjectName
	}
	for _, segment := range strings.Split(key, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return errInvalidObjectName
		}
	}
	objectRoot := "/" + bucket + "/"
	if !strings.HasPrefix(path.Clean(objectRoot+key), objectRoot) {
		return errInvalidObjectName
	}
	return nil
}

func objectNameError(cause error) *s3base.APIError {
	return s3base.NewError(
		http.StatusBadRequest,
		"InvalidObjectName",
		"The specified object key is invalid.",
		cause,
	)
}

func objectError(err error, bucket, key, resource string) *s3base.APIError {
	var apiError *s3base.APIError
	if errors.As(err, &apiError) {
		return apiError
	}
	if errors.Is(err, os.ErrNotExist) {
		apiError = s3base.NoSuchKey(err)
	} else {
		apiError = s3base.InternalError(err)
	}
	apiError.Bucket = bucket
	apiError.Key = key
	apiError.Resource = resource
	return apiError
}

func setObjectHeaders(c *gin.Context, info *filemgr.S3ObjectInfo, includeChecksum bool) {
	metadata := info.Metadata
	c.Header("ETag", metadata.ETag)
	c.Header("Last-Modified", time.UnixMilli(info.Link.Mtime).UTC().Format(http.TimeFormat))
	c.Header("Content-Type", metadata.ContentType)
	c.Header("Cache-Control", metadata.CacheControl)
	setOptionalHeader(c, "Content-Disposition", metadata.ContentDisposition)
	setOptionalHeader(c, "Content-Encoding", metadata.ContentEncoding)
	setOptionalHeader(c, "Content-Language", metadata.ContentLanguage)
	setOptionalHeader(c, "Expires", metadata.Expires)
	if includeChecksum && metadata.ChecksumSHA256 != "" {
		c.Header("x-amz-checksum-sha256", metadata.ChecksumSHA256)
	}
	if includeChecksum && metadata.RequestChecksumAlgorithm != "" {
		c.Header(checksumHeader(metadata.RequestChecksumAlgorithm), metadata.RequestChecksumValue)
	}
	if includeChecksum && metadata.ChecksumType != "" {
		c.Header("x-amz-checksum-type", metadata.ChecksumType)
	}
	var userMetadata map[string]string
	if err := json.Unmarshal([]byte(metadata.UserMetadata), &userMetadata); err == nil {
		for key, value := range userMetadata {
			c.Header("x-amz-meta-"+key, value)
		}
	}
}

func setOptionalHeader(c *gin.Context, name, value string) {
	if value != "" {
		c.Header(name, value)
	}
}

func checkReadConditions(request *http.Request, info *filemgr.S3ObjectInfo) *s3base.APIError {
	if apiError := checkReadETagConditions(request, info); apiError != nil {
		return apiError
	}
	return checkReadDateConditions(request, info)
}

func checkReadETagConditions(request *http.Request, info *filemgr.S3ObjectInfo) *s3base.APIError {
	ifMatch := request.Header.Get("If-Match")
	if ifMatch != "" && !validETagList(ifMatch) {
		return s3base.NewError(http.StatusBadRequest, "InvalidArgument", "If-Match is invalid.", nil)
	}
	if ifMatch != "" && ifMatch != "*" &&
		(strings.HasPrefix(info.Metadata.ETag, "W/") || !etagListContains(ifMatch, info.Metadata.ETag, false)) {
		return s3base.PreconditionFailed(nil)
	}
	ifNoneMatch := request.Header.Get("If-None-Match")
	if ifNoneMatch != "" && !validETagList(ifNoneMatch) {
		return s3base.NewError(http.StatusBadRequest, "InvalidArgument", "If-None-Match is invalid.", nil)
	}
	if ifNoneMatch != "" && (ifNoneMatch == "*" || etagListContains(ifNoneMatch, info.Metadata.ETag, true)) {
		return s3base.NewError(http.StatusNotModified, "NotModified", "Not Modified.", nil)
	}
	return nil
}

func checkReadDateConditions(request *http.Request, info *filemgr.S3ObjectInfo) *s3base.APIError {
	modifiedAt := time.UnixMilli(info.Link.Mtime).Truncate(time.Second)
	ifMatch := request.Header.Get("If-Match")
	if ifMatch == "" {
		if value := request.Header.Get("If-Unmodified-Since"); value != "" {
			if timestamp, err := http.ParseTime(value); err == nil &&
				modifiedAt.After(timestamp.Truncate(time.Second)) {
				return s3base.PreconditionFailed(nil)
			}
		}
	}
	ifNoneMatch := request.Header.Get("If-None-Match")
	if ifNoneMatch == "" {
		if value := request.Header.Get("If-Modified-Since"); value != "" {
			if timestamp, err := http.ParseTime(value); err == nil &&
				!modifiedAt.After(timestamp.Truncate(time.Second)) {
				return s3base.NewError(http.StatusNotModified, "NotModified", "Not Modified.", nil)
			}
		}
	}
	return nil
}

func validETagList(value string) bool {
	if value == "*" {
		return true
	}
	parts := strings.Split(value, ",")
	for _, part := range parts {
		candidate := strings.TrimSpace(part)
		candidate = strings.TrimPrefix(candidate, "W/")
		if !validSingleETag(candidate) {
			return false
		}
	}
	return len(parts) != 0
}

func validateObjectRange(request *http.Request, info *filemgr.S3ObjectInfo) *s3base.APIError {
	value := request.Header.Get("Range")
	if value == "" || !objectRangeApplies(request, info) {
		return nil
	}
	if !strings.HasPrefix(value, "bytes=") || strings.ContainsAny(value, ", \t") {
		return s3base.InvalidRange(errInvalidObjectRange)
	}
	specification := strings.TrimPrefix(value, "bytes=")
	startText, endText, found := strings.Cut(specification, "-")
	if !found || startText == "" && endText == "" || info.Link.FileSize == 0 {
		return s3base.InvalidRange(errInvalidObjectRange)
	}
	if startText == "" {
		return validateSuffixRange(endText)
	}
	return validateStartRange(startText, endText, info.Link.FileSize)
}

func validateSuffixRange(endText string) *s3base.APIError {
	suffix, err := strconv.ParseInt(endText, 10, 64)
	if err != nil || suffix <= 0 {
		return s3base.InvalidRange(errors.Join(errInvalidObjectRange, err))
	}
	return nil
}

func validateStartRange(startText, endText string, fileSize int64) *s3base.APIError {
	start, err := strconv.ParseInt(startText, 10, 64)
	if err != nil || start < 0 || start >= fileSize {
		return s3base.InvalidRange(errors.Join(errInvalidObjectRange, err))
	}
	if endText == "" {
		return nil
	}
	end, err := strconv.ParseInt(endText, 10, 64)
	if err != nil || end < start {
		return s3base.InvalidRange(errors.Join(errInvalidObjectRange, err))
	}
	return nil
}

func objectRangeApplies(request *http.Request, info *filemgr.S3ObjectInfo) bool {
	value := request.Header.Get("If-Range")
	if value == "" {
		return true
	}
	if strings.HasPrefix(value, `"`) || strings.HasPrefix(value, "W/") {
		return !strings.HasPrefix(info.Metadata.ETag, "W/") && value == info.Metadata.ETag
	}
	timestamp, err := http.ParseTime(value)
	if err != nil {
		return false
	}
	modified := time.UnixMilli(info.Link.Mtime).Truncate(time.Second)
	return !modified.After(timestamp.Truncate(time.Second))
}

func etagListContains(headerValue, etag string, weak bool) bool {
	for _, candidate := range strings.Split(headerValue, ",") {
		candidate = strings.TrimSpace(candidate)
		if weak {
			candidate = strings.TrimPrefix(candidate, "W/")
			etag = strings.TrimPrefix(etag, "W/")
		}
		if candidate == etag {
			return true
		}
	}
	return false
}

func parseDestinationCondition(request *http.Request) (*filemgr.S3Condition, *s3base.APIError) {
	ifMatch := request.Header.Get("If-Match")
	ifNoneMatch := request.Header.Get("If-None-Match")
	if ifMatch != "" && ifNoneMatch != "" {
		return nil, s3base.InvalidRequest("If-Match and If-None-Match cannot be combined.", nil)
	}
	if ifNoneMatch != "" && ifNoneMatch != "*" {
		return nil, s3base.InvalidRequest("PutObject only supports If-None-Match: *.", nil)
	}
	if ifMatch != "" && ifMatch != "*" && !validSingleETag(ifMatch) {
		return nil, s3base.InvalidRequest("If-Match must contain one valid ETag.", nil)
	}
	return &filemgr.S3Condition{IfMatch: ifMatch, IfNoneMatch: ifNoneMatch}, nil
}

func validSingleETag(value string) bool {
	return len(value) >= 2 && strings.HasPrefix(value, `"`) && strings.HasSuffix(value, `"`) &&
		!strings.Contains(value[1:len(value)-1], `"`)
}

func (h *S3Handler) fastCheckCondition(
	ctx context.Context,
	objectPath string,
	condition *filemgr.S3Condition,
) *s3base.APIError {
	info, err := h.fmgr.StatS3Object(ctx, objectPath)
	exists := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return s3base.InternalError(err)
	}
	if condition.IfNoneMatch == "*" && exists {
		return s3base.PreconditionFailed(filemgr.ErrS3Precondition)
	}
	if condition.IfMatch != "" {
		if !exists || condition.IfMatch != "*" &&
			(strings.HasPrefix(info.Metadata.ETag, "W/") || info.Metadata.ETag != condition.IfMatch) {
			return s3base.PreconditionFailed(filemgr.ErrS3Precondition)
		}
	}
	return nil
}

func parseRequestMetadata(request *http.Request, key string) (*entity.S3ObjectMetadata, *s3base.APIError) {
	contentType := request.Header.Get("Content-Type")
	if contentType == "" {
		extension := path.Ext(key)
		contentType = mimetype.LookupWithDefault(extension, "application/octet-stream")
	}
	cacheControl := request.Header.Get("Cache-Control")
	if cacheControl == "" {
		cacheControl = defaultObjectCacheControl
	}
	expires := request.Header.Get("Expires")
	if expires != "" {
		parsed, err := http.ParseTime(expires)
		if err != nil {
			return nil, s3base.NewError(
				http.StatusBadRequest,
				"InvalidArgument",
				"Expires must be a valid HTTP date.",
				err,
			)
		}
		expires = parsed.UTC().Format(http.TimeFormat)
	}
	userMetadata := make(map[string]string)
	total := 0
	for name, values := range request.Header {
		lower := strings.ToLower(name)
		if !strings.HasPrefix(lower, "x-amz-meta-") {
			continue
		}
		key := strings.TrimPrefix(lower, "x-amz-meta-")
		value := strings.Join(values, ",")
		if key == "" || len(key) > maxMetadataKeySize || hasControlCharacter(key) {
			return nil, metadataError("InvalidArgument", "User metadata key is invalid.")
		}
		if len(value) > maxMetadataValue || hasControlCharacter(value) {
			return nil, metadataError("InvalidArgument", "User metadata value is invalid.")
		}
		total += len(key) + len(value)
		if total > maxMetadataTotal {
			return nil, metadataError("MetadataTooLarge", "User metadata is too large.")
		}
		userMetadata[key] = value
	}
	rawMetadata, err := json.Marshal(userMetadata)
	if err != nil {
		return nil, s3base.InternalError(err)
	}
	return &entity.S3ObjectMetadata{
		ContentType:        contentType,
		CacheControl:       cacheControl,
		ContentDisposition: request.Header.Get("Content-Disposition"),
		ContentEncoding:    removeAWSChunked(request.Header.Get("Content-Encoding")),
		ContentLanguage:    request.Header.Get("Content-Language"),
		Expires:            expires,
		UserMetadata:       string(rawMetadata),
	}, nil
}

func hasControlCharacter(value string) bool {
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return true
		}
	}
	return false
}

func metadataError(code, message string) *s3base.APIError {
	return s3base.NewError(http.StatusBadRequest, code, message, nil)
}

func removeAWSChunked(value string) string {
	parts := strings.Split(value, ",")
	filtered := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if !strings.EqualFold(part, "aws-chunked") && part != "" {
			filtered = append(filtered, part)
		}
	}
	return strings.Join(filtered, ",")
}

func containsContentEncoding(value, wanted string) bool {
	for _, part := range strings.Split(value, ",") {
		if strings.EqualFold(strings.TrimSpace(part), wanted) {
			return true
		}
	}
	return false
}

type uploadHashes struct {
	md5            hash.Hash
	sha256         hash.Hash
	request        hash.Hash
	algorithm      s3checksum.Algorithm
	headerExpected string
	expected       string
	contentMD5     string
	trailer        string
}

func newUploadHashes(request *http.Request) (*uploadHashes, io.Reader, *s3base.APIError) {
	checksum, apiError := parseChecksumRequest(request)
	if apiError != nil {
		return nil, nil, apiError
	}
	return buildUploadHashes(request, checksum, false)
}

func newMultipartUploadHashes(
	request *http.Request,
	spec *filemgr.MultipartChecksumSpec,
) (*uploadHashes, io.Reader, *s3base.APIError) {
	checksum, apiError := parseChecksumRequest(request)
	if apiError != nil {
		return nil, nil, apiError
	}
	if spec.Legacy {
		if checksum.algorithm != "" {
			return nil, nil, multipartNotImplemented(
				"Additional checksums are not available for this legacy multipart upload.",
			)
		}
		return buildUploadHashes(request, checksum, false)
	}
	if checksum.algorithm != "" && checksum.algorithm != spec.Algorithm {
		return nil, nil, s3base.InvalidRequest(
			"The checksum algorithm must match CreateMultipartUpload.",
			nil,
		)
	}
	checksum.algorithm = spec.Algorithm
	if spec.ChecksumType == s3checksum.TypeComposite &&
		checksum.headerValue == "" &&
		checksum.trailerName == "" {
		return nil, nil, s3base.NewError(
			http.StatusBadRequest,
			"InvalidPart",
			"A checksum is required for every composite multipart part.",
			nil,
		)
	}
	return buildUploadHashes(request, checksum, true)
}

func buildUploadHashes(
	request *http.Request,
	checksum *checksumRequest,
	forceChecksum bool,
) (*uploadHashes, io.Reader, *s3base.APIError) {
	hashes := &uploadHashes{md5: filemgr.NewMD5CompatibilityHash(), sha256: sha256.New()}
	writers := []io.Writer{hashes.md5, hashes.sha256}
	contentMD5 := request.Header.Get("Content-MD5")
	if checksum.algorithm != "" {
		requestHash, err := s3checksum.NewHash(checksum.algorithm)
		if err != nil {
			return nil, nil, s3base.InternalError(err)
		}
		hashes.request = requestHash
		hashes.algorithm = checksum.algorithm
		hashes.headerExpected = checksum.headerValue
		hashes.expected = checksum.headerValue
		hashes.trailer = checksum.trailerName
		writers = append(writers, hashes.request)
	} else if forceChecksum {
		return nil, nil, s3base.InternalError(errEmptyMultipartChecksumAlgorithm)
	}
	reader := io.TeeReader(request.Body, io.MultiWriter(writers...))
	if contentMD5 != "" {
		decoded, err := base64.StdEncoding.DecodeString(contentMD5)
		if err != nil || len(decoded) != filemgr.MD5CompatibilitySize {
			return nil, nil, s3base.NewError(
				http.StatusBadRequest,
				"InvalidDigest",
				"The Content-MD5 value is invalid.",
				err,
			)
		}
		hashes.contentMD5 = contentMD5
	}
	return hashes, reader, nil
}

func checksumHeader(algorithm string) string {
	name, err := s3checksum.HeaderName(s3checksum.Algorithm(algorithm))
	if err != nil {
		return ""
	}
	return name
}

func (h *uploadHashes) validate() *s3base.APIError {
	if h.contentMD5 != "" {
		actual := base64.StdEncoding.EncodeToString(h.md5.Sum(nil))
		if actual != h.contentMD5 {
			return s3base.NewError(http.StatusBadRequest, "BadDigest", "The Content-MD5 did not match.", errChecksumMismatch)
		}
	}
	if h.request != nil {
		actual := s3checksum.SumBase64(h.request)
		if h.expected != "" && actual != h.expected {
			return s3base.NewError(
				http.StatusBadRequest,
				"BadDigest",
				"The request checksum did not match.",
				errChecksumMismatch,
			)
		}
	}
	return nil
}

func (h *uploadHashes) loadTrailer(c *gin.Context) *s3base.APIError {
	if h.trailer == "" {
		return nil
	}
	value, exists := c.Get("s3-verified-trailers")
	if !exists {
		return s3base.InvalidRequest("The declared checksum trailer was not verified.", nil)
	}
	trailers, ok := value.(http.Header)
	if !ok {
		return s3base.InternalError(errVerifiedTrailerType)
	}
	expected := trailers.Get(h.trailer)
	if _, err := s3checksum.Decode(h.algorithm, expected); err != nil {
		return invalidChecksumDigest(err)
	}
	if h.headerExpected != "" && h.headerExpected != expected {
		return s3base.NewError(
			http.StatusBadRequest,
			"BadDigest",
			"The checksum header and trailer did not match.",
			errChecksumMismatch,
		)
	}
	h.expected = expected
	return nil
}

func (h *uploadHashes) checksumValue() string {
	if h.request == nil {
		return ""
	}
	return s3checksum.SumBase64(h.request)
}

func uploadError(err error) *s3base.APIError {
	var apiError *s3base.APIError
	if errors.As(err, &apiError) {
		return apiError
	}
	if errors.Is(err, filemgr.ErrInvalidFileSize) ||
		errors.Is(err, filemgr.ErrTooManyFileParts) {
		return s3base.NewError(http.StatusBadRequest, "EntityTooLarge", "The object is too large.", err)
	}
	if errors.Is(err, filemgr.ErrFileShortRead) {
		return s3base.NewError(http.StatusBadRequest, "IncompleteBody", "The request body was incomplete.", err)
	}
	return verifierBodyError(err)
}

func verifierBodyError(err error) *s3base.APIError {
	apiError := verifierAPIError(err)
	if apiError.Code != "InternalError" {
		return apiError
	}
	return s3base.InternalError(err)
}

func mutationError(err error) *s3base.APIError {
	if errors.Is(err, filemgr.ErrS3Precondition) {
		return s3base.PreconditionFailed(err)
	}
	if errors.Is(err, filemgr.ErrS3ObjectConflict) || errors.Is(err, filemgr.ErrMultipartConflict) {
		return s3base.NewError(
			http.StatusConflict,
			"ConditionalRequestConflict",
			"A conflicting operation occurred.",
			err,
		)
	}
	return s3base.InternalError(err)
}

func rejectObjectACLHeaders(request *http.Request) *s3base.APIError {
	if request.Header.Get("X-Amz-Acl") != "" {
		return s3base.NewError(
			http.StatusBadRequest,
			"AccessControlListNotSupported",
			"The bucket does not allow object ACLs.",
			nil,
		)
	}
	for name := range request.Header {
		if strings.HasPrefix(strings.ToLower(name), "x-amz-grant-") {
			return s3base.NewError(
				http.StatusBadRequest,
				"AccessControlListNotSupported",
				"The bucket does not allow object ACLs.",
				nil,
			)
		}
	}
	return nil
}

func decodeCopySource(raw string) (string, string, *s3base.APIError) {
	decoded, err := url.PathUnescape(raw)
	if err != nil {
		return "", "", s3base.InvalidRequest("x-amz-copy-source is invalid.", err)
	}
	bucket, key := requestBucketKey(decoded)
	if bucket == "" || key == "" {
		return "", "", s3base.InvalidRequest("x-amz-copy-source must contain a bucket and key.", nil)
	}
	return bucket, key, nil
}

func logCloseError(ctx context.Context, closer io.Closer, message string) {
	if err := closer.Close(); err != nil {
		logutil.GetLogger(ctx).Error(message, zap.Error(err))
	}
}
