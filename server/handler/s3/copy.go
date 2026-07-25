package s3

import (
	"encoding/xml"
	"errors"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/xxxsen/tgfile/entity"
	"github.com/xxxsen/tgfile/filemgr"
	"github.com/xxxsen/tgfile/s3checksum"
	"github.com/xxxsen/tgfile/server/handler/s3/s3base"

	"github.com/gin-gonic/gin"
)

type copyObjectResult struct {
	XMLName xml.Name `xml:"CopyObjectResult"`
	XMLNS   string   `xml:"xmlns,attr"`
	checksumXMLFields
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	ChecksumType string `xml:"ChecksumType,omitempty"`
}

func (h *S3Handler) CopyObject(c *gin.Context) {
	preparation, apiError := h.prepareCopyPaths(c)
	if apiError != nil {
		s3base.WriteError(c, apiError)
		return
	}
	unlock := h.lockPaths(preparation.sourcePath, preparation.destinationPath)
	defer unlock()
	sourceInfo, sourceCondition, destinationCondition, replacement, apiError := h.prepareCopyOperation(c, preparation)
	if apiError != nil {
		s3base.WriteError(c, apiError)
		return
	}
	if apiError := validateCopyChecksumRequest(c.Request, sourceInfo); apiError != nil {
		s3base.WriteError(c, apiError)
		return
	}
	result, err := h.fmgr.CopyS3Object(
		c.Request.Context(),
		preparation.sourcePath,
		preparation.destinationPath,
		replacement,
		sourceCondition,
		destinationCondition,
	)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s3base.WriteError(c, s3base.NoSuchKey(err))
			return
		}
		s3base.WriteError(c, mutationError(err))
		return
	}
	response := &copyObjectResult{
		XMLNS:        s3XMLNamespace,
		LastModified: time.UnixMilli(result.Link.Mtime).UTC().Format("2006-01-02T15:04:05.000Z"),
		ETag:         result.Metadata.ETag,
		ChecksumType: result.Metadata.ChecksumType,
	}
	setChecksumXML(
		&response.checksumXMLFields,
		s3checksum.Algorithm(result.Metadata.RequestChecksumAlgorithm),
		result.Metadata.RequestChecksumValue,
	)
	c.XML(http.StatusOK, response)
}

type copyPreparation struct {
	sourcePath      string
	destinationPath string
	destinationKey  string
}

func (h *S3Handler) prepareCopyPaths(c *gin.Context) (*copyPreparation, *s3base.APIError) {
	destinationBucket, destinationKey, apiError := h.authorizeWrite(c)
	if apiError != nil {
		return nil, apiError
	}
	if apiError := rejectObjectACLHeaders(c.Request); apiError != nil {
		return nil, apiError
	}
	if err := validateNewObjectKey(destinationKey); err != nil {
		return nil, objectNameError(err)
	}
	sourceBucketName, sourceKey, apiError := decodeCopySource(c.GetHeader("x-amz-copy-source"))
	if apiError != nil {
		return nil, apiError
	}
	if _, exists := h.Bucket(sourceBucketName); !exists {
		return nil, s3base.NewError(
			http.StatusNotFound,
			"NoSuchBucket",
			"The specified source bucket does not exist.",
			nil,
		)
	}
	if err := validateHistoricalObjectKeyBoundary(sourceBucketName, sourceKey); err != nil {
		return nil, objectNameError(err)
	}
	return &copyPreparation{
		sourcePath:      "/" + sourceBucketName + "/" + sourceKey,
		destinationPath: "/" + destinationBucket.Name + "/" + destinationKey,
		destinationKey:  destinationKey,
	}, nil
}

func (h *S3Handler) prepareCopyOperation(
	c *gin.Context,
	preparation *copyPreparation,
) (
	*filemgr.S3ObjectInfo,
	*filemgr.S3Condition,
	*filemgr.S3Condition,
	*entity.S3ObjectMetadata,
	*s3base.APIError,
) {
	sourceInfo, err := h.fmgr.StatS3Object(c.Request.Context(), preparation.sourcePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, nil, nil, s3base.NoSuchKey(err)
		}
		return nil, nil, nil, nil, s3base.InternalError(err)
	}
	sourceCondition, apiError := parseCopySourceCondition(c.Request)
	if apiError != nil {
		return nil, nil, nil, nil, apiError
	}
	if apiError := checkConditionAgainstInfo(sourceCondition, sourceInfo); apiError != nil {
		return nil, nil, nil, nil, apiError
	}
	destinationCondition, apiError := parseDestinationCondition(c.Request)
	if apiError != nil {
		return nil, nil, nil, nil, apiError
	}
	replacement, apiError := copyReplacementMetadata(c, preparation.destinationKey, sourceInfo)
	if apiError != nil {
		return nil, nil, nil, nil, apiError
	}
	return sourceInfo, sourceCondition, destinationCondition, replacement, nil
}

func copyReplacementMetadata(
	c *gin.Context,
	destinationKey string,
	sourceInfo *filemgr.S3ObjectInfo,
) (*entity.S3ObjectMetadata, *s3base.APIError) {
	directive := strings.ToUpper(c.GetHeader("x-amz-metadata-directive"))
	if directive == "" || directive == "COPY" {
		return nil, nil
	}
	if directive != "REPLACE" {
		return nil, s3base.NewError(
			http.StatusBadRequest,
			"InvalidArgument",
			"x-amz-metadata-directive must be COPY or REPLACE.",
			nil,
		)
	}
	replacement, apiError := parseRequestMetadata(c.Request, destinationKey)
	if apiError != nil {
		return nil, apiError
	}
	replacement.ETag = sourceInfo.Metadata.ETag
	replacement.ChecksumSHA256 = sourceInfo.Metadata.ChecksumSHA256
	replacement.RequestChecksumAlgorithm = sourceInfo.Metadata.RequestChecksumAlgorithm
	replacement.RequestChecksumValue = sourceInfo.Metadata.RequestChecksumValue
	replacement.ChecksumType = sourceInfo.Metadata.ChecksumType
	return replacement, nil
}

func (h *S3Handler) lockPaths(paths ...string) func() {
	unique := make(map[string]struct{}, len(paths))
	for _, objectPath := range paths {
		unique[objectPath] = struct{}{}
	}
	ordered := make([]string, 0, len(unique))
	for objectPath := range unique {
		ordered = append(ordered, objectPath)
	}
	slices.Sort(ordered)
	unlocks := make([]func(), 0, len(ordered))
	for _, objectPath := range ordered {
		unlocks = append(unlocks, h.locks.lock(objectPath))
	}
	return func() {
		for index := len(unlocks) - 1; index >= 0; index-- {
			unlocks[index]()
		}
	}
}

func parseCopySourceCondition(request *http.Request) (*filemgr.S3Condition, *s3base.APIError) {
	condition := &filemgr.S3Condition{
		IfMatch:     request.Header.Get("X-Amz-Copy-Source-If-Match"),
		IfNoneMatch: request.Header.Get("X-Amz-Copy-Source-If-None-Match"),
	}
	if condition.IfMatch != "" && condition.IfNoneMatch != "" {
		return nil, s3base.InvalidRequest("Conflicting copy source conditions.", nil)
	}
	if condition.IfMatch != "" && !validETagList(condition.IfMatch) {
		return nil, s3base.NewError(
			http.StatusBadRequest,
			"InvalidArgument",
			"x-amz-copy-source-if-match is invalid.",
			nil,
		)
	}
	if condition.IfNoneMatch != "" && !validETagList(condition.IfNoneMatch) {
		return nil, s3base.NewError(
			http.StatusBadRequest,
			"InvalidArgument",
			"x-amz-copy-source-if-none-match is invalid.",
			nil,
		)
	}
	var apiError *s3base.APIError
	condition.IfModifiedSince, apiError = parseOptionalHTTPDate(
		request.Header.Get("X-Amz-Copy-Source-If-Modified-Since"),
	)
	if apiError != nil {
		return nil, apiError
	}
	condition.IfUnmodifiedSince, apiError = parseOptionalHTTPDate(
		request.Header.Get("X-Amz-Copy-Source-If-Unmodified-Since"),
	)
	if apiError != nil {
		return nil, apiError
	}
	return condition, nil
}

func parseOptionalHTTPDate(value string) (*time.Time, *s3base.APIError) {
	if value == "" {
		return nil, nil
	}
	parsed, err := http.ParseTime(value)
	if err != nil {
		return nil, s3base.NewError(http.StatusBadRequest, "InvalidArgument", "A condition date is invalid.", err)
	}
	return &parsed, nil
}

func checkConditionAgainstInfo(
	condition *filemgr.S3Condition,
	info *filemgr.S3ObjectInfo,
) *s3base.APIError {
	if condition.IfMatch != "" && condition.IfMatch != "*" &&
		(strings.HasPrefix(info.Metadata.ETag, "W/") ||
			!etagListContains(condition.IfMatch, info.Metadata.ETag, false)) {
		return s3base.PreconditionFailed(nil)
	}
	if condition.IfNoneMatch != "" &&
		(condition.IfNoneMatch == "*" || etagListContains(condition.IfNoneMatch, info.Metadata.ETag, true)) {
		return s3base.PreconditionFailed(nil)
	}
	modified := time.UnixMilli(info.Link.Mtime).Truncate(time.Second)
	if condition.IfMatch == "" && condition.IfUnmodifiedSince != nil &&
		modified.After(condition.IfUnmodifiedSince.Truncate(time.Second)) {
		return s3base.PreconditionFailed(nil)
	}
	if condition.IfNoneMatch == "" && condition.IfModifiedSince != nil &&
		!modified.After(condition.IfModifiedSince.Truncate(time.Second)) {
		return s3base.PreconditionFailed(nil)
	}
	return nil
}

func validateCopyChecksumRequest(
	request *http.Request,
	source *filemgr.S3ObjectInfo,
) *s3base.APIError {
	algorithm := strings.ToUpper(request.Header.Get("X-Amz-Checksum-Algorithm"))
	if algorithm == "" {
		return nil
	}
	if algorithm == "SHA256" && source.Metadata.ChecksumSHA256 != "" {
		return nil
	}
	if algorithm == source.Metadata.RequestChecksumAlgorithm &&
		source.Metadata.RequestChecksumValue != "" {
		return nil
	}
	return s3base.NewError(
		http.StatusNotImplemented,
		"NotImplemented",
		"The requested checksum cannot be reused without reading object content.",
		nil,
	)
}
