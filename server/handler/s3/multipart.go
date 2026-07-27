package s3

import (
	"bytes"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/xxxsen/tgfile/authz"
	"github.com/xxxsen/tgfile/filemgr"
	"github.com/xxxsen/tgfile/s3checksum"
	"github.com/xxxsen/tgfile/server/handler/s3/s3base"

	"github.com/gin-gonic/gin"
	"github.com/xxxsen/s3verify"
)

const (
	maxMultipartPartNumber    = 10_000
	maxMultipartPartBytes     = int64(5 * 1024 * 1024 * 1024)
	maxMultipartCompleteBody  = int64(2 * 1024 * 1024)
	defaultMultipartListLimit = 1000
	multipartUploadIDLength   = 64
)

var (
	errMalformedCompleteXML = errors.New("malformed CompleteMultipartUpload XML")
	errMultipartBodyTooLong = errors.New("multipart completion body is too large")
)

type initiateMultipartUploadResult struct {
	XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
	XMLNS    string   `xml:"xmlns,attr"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	UploadID string   `xml:"UploadId"`
}

type listPartsResult struct {
	XMLName              xml.Name         `xml:"ListPartsResult"`
	XMLNS                string           `xml:"xmlns,attr"`
	Bucket               string           `xml:"Bucket"`
	Key                  string           `xml:"Key"`
	UploadID             string           `xml:"UploadId"`
	PartNumberMarker     int              `xml:"PartNumberMarker"`
	NextPartNumberMarker int              `xml:"NextPartNumberMarker,omitempty"`
	MaxParts             int              `xml:"MaxParts"`
	IsTruncated          bool             `xml:"IsTruncated"`
	ChecksumAlgorithm    string           `xml:"ChecksumAlgorithm,omitempty"`
	ChecksumType         string           `xml:"ChecksumType,omitempty"`
	Parts                []listedPartItem `xml:"Part,omitempty"`
}

type listedPartItem struct {
	PartNumber   int    `xml:"PartNumber"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
	checksumXMLFields
}

type completeMultipartUploadResult struct {
	XMLName      xml.Name `xml:"CompleteMultipartUploadResult"`
	XMLNS        string   `xml:"xmlns,attr"`
	Location     string   `xml:"Location"`
	Bucket       string   `xml:"Bucket"`
	Key          string   `xml:"Key"`
	ETag         string   `xml:"ETag"`
	ChecksumType string   `xml:"ChecksumType,omitempty"`
	checksumXMLFields
}

type checksumXMLFields struct {
	ChecksumCRC32     string `xml:"ChecksumCRC32,omitempty"`
	ChecksumCRC32C    string `xml:"ChecksumCRC32C,omitempty"`
	ChecksumCRC64NVME string `xml:"ChecksumCRC64NVME,omitempty"`
	ChecksumSHA1      string `xml:"ChecksumSHA1,omitempty"`
	ChecksumSHA256    string `xml:"ChecksumSHA256,omitempty"`
}

type listMultipartUploadsResult struct {
	XMLName            xml.Name                    `xml:"ListMultipartUploadsResult"`
	XMLNS              string                      `xml:"xmlns,attr"`
	Bucket             string                      `xml:"Bucket"`
	KeyMarker          string                      `xml:"KeyMarker"`
	UploadIDMarker     string                      `xml:"UploadIdMarker,omitempty"`
	NextKeyMarker      string                      `xml:"NextKeyMarker,omitempty"`
	NextUploadIDMarker string                      `xml:"NextUploadIdMarker,omitempty"`
	Delimiter          string                      `xml:"Delimiter,omitempty"`
	Prefix             string                      `xml:"Prefix"`
	EncodingType       string                      `xml:"EncodingType,omitempty"`
	MaxUploads         int                         `xml:"MaxUploads"`
	IsTruncated        bool                        `xml:"IsTruncated"`
	Uploads            []listedMultipartUploadItem `xml:"Upload,omitempty"`
	CommonPrefixes     []commonPrefix              `xml:"CommonPrefixes,omitempty"`
}

type listedMultipartUploadItem struct {
	Key               string    `xml:"Key"`
	UploadID          string    `xml:"UploadId"`
	Initiator         listOwner `xml:"Initiator"`
	Owner             listOwner `xml:"Owner"`
	StorageClass      string    `xml:"StorageClass"`
	Initiated         string    `xml:"Initiated"`
	ChecksumAlgorithm string    `xml:"ChecksumAlgorithm,omitempty"`
	ChecksumType      string    `xml:"ChecksumType,omitempty"`
}

type uploadPartPreparation struct {
	bucket     Bucket
	key        string
	uploadID   string
	partNumber int
	size       int64
}

func (h *S3Handler) CreateMultipartUpload(c *gin.Context) {
	bucket, key, apiError := h.authorizeWrite(c)
	if apiError != nil {
		s3base.WriteError(c, apiError)
		return
	}
	if apiError := validateMultipartQuery(c.Request.URL.Query(), []string{"uploads"}, nil); apiError != nil {
		s3base.WriteError(c, apiError)
		return
	}
	if c.Request.URL.Query().Get("uploads") != "" {
		s3base.WriteError(c, invalidMultipartArgument("uploads must not have a value.", nil))
		return
	}
	if err := validateNewObjectKey(key); err != nil {
		s3base.WriteError(c, objectNameError(err))
		return
	}
	if apiError := rejectUnsupportedMultipartHeaders(c.Request); apiError != nil {
		s3base.WriteError(c, apiError)
		return
	}
	algorithm, checksumType, apiError := parseCreateMultipartChecksum(c.Request)
	if apiError != nil {
		s3base.WriteError(c, apiError)
		return
	}
	storageClass := c.GetHeader("x-amz-storage-class")
	if storageClass != "" && storageClass != "STANDARD" {
		s3base.WriteError(c, s3base.NewError(
			http.StatusBadRequest,
			"InvalidStorageClass",
			"The storage class you specified is not valid.",
			nil,
		))
		return
	}
	metadata, apiError := parseRequestMetadata(c.Request, key)
	if apiError != nil {
		s3base.WriteError(c, apiError)
		return
	}
	upload, err := h.fmgr.CreateMultipartUpload(c.Request.Context(), &filemgr.CreateMultipartRequest{
		Bucket:       bucket.Name,
		Key:          key,
		Metadata:     metadata,
		ExpireAfter:  h.multipartExpiry,
		Algorithm:    algorithm,
		ChecksumType: checksumType,
	})
	if err != nil {
		s3base.WriteError(c, multipartError(err))
		return
	}
	c.Header("x-amz-checksum-algorithm", string(upload.Algorithm))
	c.Header("x-amz-checksum-type", string(upload.ChecksumType))
	c.XML(http.StatusOK, &initiateMultipartUploadResult{
		XMLNS:    s3XMLNamespace,
		Bucket:   bucket.Name,
		Key:      key,
		UploadID: upload.UploadID,
	})
}

func (h *S3Handler) UploadPart(c *gin.Context) {
	preparation, apiError := h.prepareUploadPart(c)
	if apiError != nil {
		s3base.WriteError(c, apiError)
		return
	}
	unlock := h.multipartLocks.lock(preparation.uploadID)
	defer unlock()
	spec, err := h.fmgr.PrepareMultipartPart(
		c.Request.Context(),
		&filemgr.PrepareMultipartPartRequest{
			UploadID: preparation.uploadID,
			Bucket:   preparation.bucket.Name,
			Key:      preparation.key,
		},
	)
	if err != nil {
		s3base.WriteError(c, multipartError(err))
		return
	}
	fileID, hashes, apiError := h.receiveMultipartUpload(c, preparation.size, spec)
	if apiError != nil {
		s3base.WriteError(c, apiError)
		return
	}
	etag := hex.EncodeToString(hashes.md5.Sum(nil))
	_, err = h.fmgr.PutMultipartPart(c.Request.Context(), &filemgr.PutMultipartPartRequest{
		UploadID:      preparation.uploadID,
		Bucket:        preparation.bucket.Name,
		Key:           preparation.key,
		PartNumber:    preparation.partNumber,
		FileID:        fileID,
		Size:          preparation.size,
		ETag:          etag,
		ChecksumValue: hashes.checksumValue(),
		MaxObjectSize: h.maxObjectSize,
	})
	if err != nil {
		discardUploadedFile(c.Request.Context(), h.fmgr, fileID)
		s3base.WriteError(c, multipartError(err))
		return
	}
	c.Header("ETag", `"`+etag+`"`)
	if !spec.Legacy {
		header, headerErr := s3checksum.HeaderName(spec.Algorithm)
		if headerErr != nil {
			s3base.WriteError(c, s3base.InternalError(headerErr))
			return
		}
		c.Header(header, hashes.checksumValue())
	}
	c.Status(http.StatusOK)
}

func (h *S3Handler) prepareUploadPart(c *gin.Context) (*uploadPartPreparation, *s3base.APIError) {
	bucket, key, apiError := h.authorizeWrite(c)
	if apiError != nil {
		return nil, apiError
	}
	query := c.Request.URL.Query()
	if apiError := validateMultipartQuery(query, []string{"partNumber", "uploadId"}, nil); apiError != nil {
		return nil, apiError
	}
	uploadID, apiError := parseMultipartUploadID(query.Get("uploadId"))
	if apiError != nil {
		return nil, apiError
	}
	partNumber, apiError := parseBoundedMultipartInteger(
		query.Get("partNumber"),
		1,
		maxMultipartPartNumber,
		"partNumber",
	)
	if apiError != nil {
		return nil, apiError
	}
	if err := validateNewObjectKey(key); err != nil {
		return nil, objectNameError(err)
	}
	if apiError := rejectUnsupportedMultipartHeaders(c.Request); apiError != nil {
		return nil, apiError
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
	if size > maxMultipartPartBytes || h.maxObjectSize > 0 && size > h.maxObjectSize {
		return nil, multipartError(filemgr.ErrMultipartEntityTooLarge)
	}
	return &uploadPartPreparation{
		bucket:     bucket,
		key:        key,
		uploadID:   uploadID,
		partNumber: partNumber,
		size:       size,
	}, nil
}

func (h *S3Handler) ListParts(c *gin.Context) {
	bucket, key, apiError := h.authorizeObject(c, true)
	if apiError != nil {
		s3base.WriteError(c, apiError)
		return
	}
	query := c.Request.URL.Query()
	if apiError := validateMultipartQuery(
		query,
		[]string{"uploadId"},
		[]string{"part-number-marker", "max-parts"},
	); apiError != nil {
		s3base.WriteError(c, apiError)
		return
	}
	uploadID, apiError := parseMultipartUploadID(query.Get("uploadId"))
	if apiError != nil {
		s3base.WriteError(c, apiError)
		return
	}
	marker, apiError := parseOptionalMultipartInteger(
		query.Get("part-number-marker"),
		0,
		maxMultipartPartNumber,
		0,
		"part-number-marker",
	)
	if apiError != nil {
		s3base.WriteError(c, apiError)
		return
	}
	maxParts, apiError := parseOptionalMultipartInteger(
		query.Get("max-parts"),
		0,
		defaultMultipartListLimit,
		defaultMultipartListLimit,
		"max-parts",
	)
	if apiError != nil {
		s3base.WriteError(c, apiError)
		return
	}
	if err := validateNewObjectKey(key); err != nil {
		s3base.WriteError(c, objectNameError(err))
		return
	}
	unlock := h.multipartLocks.lock(uploadID)
	defer unlock()
	page, err := h.fmgr.ListMultipartParts(c.Request.Context(), &filemgr.ListMultipartPartsRequest{
		UploadID: uploadID,
		Bucket:   bucket.Name,
		Key:      key,
		Marker:   marker,
		MaxParts: maxParts,
	})
	if err != nil {
		s3base.WriteError(c, multipartError(err))
		return
	}
	c.XML(http.StatusOK, buildListPartsResult(bucket.Name, key, uploadID, marker, maxParts, page))
}

func buildListPartsResult(
	bucket, key, uploadID string,
	marker, maxParts int,
	page *filemgr.MultipartPartPage,
) *listPartsResult {
	result := &listPartsResult{
		XMLNS:                s3XMLNamespace,
		Bucket:               bucket,
		Key:                  key,
		UploadID:             uploadID,
		PartNumberMarker:     marker,
		NextPartNumberMarker: page.NextPartNumberMarker,
		MaxParts:             maxParts,
		IsTruncated:          page.IsTruncated,
		Parts:                make([]listedPartItem, 0, len(page.Parts)),
	}
	if !page.Legacy {
		result.ChecksumAlgorithm = string(page.Algorithm)
		result.ChecksumType = string(page.ChecksumType)
	}
	for _, part := range page.Parts {
		item := listedPartItem{
			PartNumber:   part.PartNumber,
			LastModified: formatS3Timestamp(part.LastModified),
			ETag:         `"` + part.ETag + `"`,
			Size:         part.Size,
		}
		if !page.Legacy {
			setChecksumXML(&item.checksumXMLFields, page.Algorithm, part.ChecksumValue)
		}
		result.Parts = append(result.Parts, item)
	}
	return result
}

func (h *S3Handler) CompleteMultipartUpload(c *gin.Context) {
	bucket, key, apiError := h.authorizeWrite(c)
	if apiError != nil {
		s3base.WriteError(c, apiError)
		return
	}
	query := c.Request.URL.Query()
	if apiError := validateMultipartQuery(query, []string{"uploadId"}, nil); apiError != nil {
		s3base.WriteError(c, apiError)
		return
	}
	uploadID, apiError := parseMultipartUploadID(query.Get("uploadId"))
	if apiError != nil {
		s3base.WriteError(c, apiError)
		return
	}
	if err := validateNewObjectKey(key); err != nil {
		s3base.WriteError(c, objectNameError(err))
		return
	}
	if apiError := rejectUnsupportedMultipartHeaders(c.Request); apiError != nil {
		s3base.WriteError(c, apiError)
		return
	}
	checksumHeaders, apiError := parseCompleteChecksumHeaders(c.Request)
	if apiError != nil {
		s3base.WriteError(c, apiError)
		return
	}
	condition, apiError := parseDestinationCondition(c.Request)
	if apiError != nil {
		s3base.WriteError(c, apiError)
		return
	}
	body, apiError := readMultipartCompleteBody(c)
	if apiError != nil {
		s3base.WriteError(c, apiError)
		return
	}
	parts, apiError := decodeMultipartCompleteBody(body)
	if apiError != nil {
		s3base.WriteError(c, apiError)
		return
	}
	unlockUpload := h.multipartLocks.lock(uploadID)
	defer unlockUpload()
	objectPath := "/" + bucket.Name + "/" + key
	unlockObject := h.locks.lock(objectPath)
	defer unlockObject()
	result, err := h.fmgr.CompleteMultipartUpload(
		c.Request.Context(),
		&filemgr.CompleteMultipartRequest{
			UploadID:               uploadID,
			Bucket:                 bucket.Name,
			Key:                    key,
			Parts:                  parts,
			MaxObjectSize:          h.maxObjectSize,
			Condition:              condition,
			FinalChecksumAlgorithm: checksumHeaders.algorithm,
			FinalChecksum:          checksumHeaders.value,
			ChecksumType:           checksumHeaders.checksumType,
			ExpectedSize:           checksumHeaders.expectedSize,
		},
	)
	if err != nil {
		s3base.WriteError(c, multipartError(err))
		return
	}
	c.Header("ETag", result.ETag)
	response := &completeMultipartUploadResult{
		XMLNS:        s3XMLNamespace,
		Location:     c.Request.URL.EscapedPath(),
		Bucket:       bucket.Name,
		Key:          key,
		ETag:         result.ETag,
		ChecksumType: string(result.ChecksumType),
	}
	setChecksumXML(&response.checksumXMLFields, result.Algorithm, result.ChecksumValue)
	c.XML(http.StatusOK, response)
}

func (h *S3Handler) AbortMultipartUpload(c *gin.Context) {
	bucket, key, apiError := h.authorizeWrite(c)
	if apiError != nil {
		s3base.WriteError(c, apiError)
		return
	}
	query := c.Request.URL.Query()
	if apiError := validateMultipartQuery(query, []string{"uploadId"}, nil); apiError != nil {
		s3base.WriteError(c, apiError)
		return
	}
	uploadID, apiError := parseMultipartUploadID(query.Get("uploadId"))
	if apiError != nil {
		s3base.WriteError(c, apiError)
		return
	}
	if err := validateNewObjectKey(key); err != nil {
		s3base.WriteError(c, objectNameError(err))
		return
	}
	unlock := h.multipartLocks.lock(uploadID)
	defer unlock()
	if err := h.fmgr.AbortMultipartUpload(c.Request.Context(), &filemgr.AbortMultipartRequest{
		UploadID: uploadID,
		Bucket:   bucket.Name,
		Key:      key,
	}); err != nil {
		s3base.WriteError(c, multipartError(err))
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *S3Handler) ListMultipartUploads(c *gin.Context) {
	bucketName, _ := requestBucketKey(c.Request.URL.Path)
	if _, exists := h.Bucket(bucketName); !exists {
		s3base.WriteError(c, noSuchBucketError(bucketName))
		return
	}
	if _, apiError := h.Authorize(c, true, authz.S3Read); apiError != nil {
		s3base.WriteError(c, apiError)
		return
	}
	request, encodingType, apiError := parseMultipartUploadListRequest(c.Request.URL.Query(), bucketName)
	if apiError != nil {
		s3base.WriteError(c, apiError)
		return
	}
	page, err := h.fmgr.ListMultipartUploads(c.Request.Context(), request)
	if err != nil {
		s3base.WriteError(c, multipartError(err))
		return
	}
	c.XML(http.StatusOK, buildMultipartUploadListResult(request, page, encodingType))
}

func parseMultipartUploadListRequest(
	query url.Values,
	bucketName string,
) (*filemgr.ListMultipartUploadsRequest, string, *s3base.APIError) {
	if apiError := validateMultipartQuery(
		query,
		[]string{"uploads"},
		[]string{"prefix", "delimiter", "key-marker", "upload-id-marker", "max-uploads", "max-keys", "encoding-type"},
	); apiError != nil {
		return nil, "", apiError
	}
	if query.Get("uploads") != "" {
		return nil, "", invalidMultipartArgument("uploads must not have a value.", nil)
	}
	if hasQueryKey(query, "max-uploads") && hasQueryKey(query, "max-keys") {
		return nil, "", invalidMultipartArgument("max-uploads and max-keys cannot be combined.", nil)
	}
	rawMax := query.Get("max-uploads")
	if rawMax == "" {
		rawMax = query.Get("max-keys")
	}
	maxUploads, apiError := parseOptionalMultipartInteger(
		rawMax,
		0,
		defaultMultipartListLimit,
		defaultMultipartListLimit,
		"max-uploads",
	)
	if apiError != nil {
		return nil, "", apiError
	}
	delimiter := query.Get("delimiter")
	if delimiter != "" && delimiter != "/" {
		return nil, "", invalidMultipartArgument("delimiter must be empty or /.", nil)
	}
	encodingType := query.Get("encoding-type")
	if encodingType != "" && encodingType != "url" {
		return nil, "", invalidMultipartArgument("encoding-type must be url.", nil)
	}
	keyMarker := query.Get("key-marker")
	uploadIDMarker := query.Get("upload-id-marker")
	if uploadIDMarker != "" && keyMarker != "" {
		if _, apiError := parseMultipartUploadID(uploadIDMarker); apiError != nil {
			return nil, "", apiError
		}
	}
	if keyMarker == "" {
		uploadIDMarker = ""
	}
	return &filemgr.ListMultipartUploadsRequest{
		Bucket:         bucketName,
		Prefix:         query.Get("prefix"),
		Delimiter:      delimiter,
		KeyMarker:      keyMarker,
		UploadIDMarker: uploadIDMarker,
		MaxUploads:     maxUploads,
	}, encodingType, nil
}

func buildMultipartUploadListResult(
	request *filemgr.ListMultipartUploadsRequest,
	page *filemgr.MultipartUploadPage,
	encodingType string,
) *listMultipartUploadsResult {
	result := &listMultipartUploadsResult{
		XMLNS:              s3XMLNamespace,
		Bucket:             request.Bucket,
		KeyMarker:          encodeListValue(request.KeyMarker, encodingType),
		UploadIDMarker:     request.UploadIDMarker,
		NextKeyMarker:      encodeListValue(page.NextKeyMarker, encodingType),
		NextUploadIDMarker: page.NextUploadIDMarker,
		Delimiter:          encodeListValue(request.Delimiter, encodingType),
		Prefix:             encodeListValue(request.Prefix, encodingType),
		EncodingType:       encodingType,
		MaxUploads:         request.MaxUploads,
		IsTruncated:        page.IsTruncated,
		Uploads:            make([]listedMultipartUploadItem, 0, len(page.Uploads)),
		CommonPrefixes:     make([]commonPrefix, 0, len(page.CommonPrefixes)),
	}
	owner := listOwner{ID: "tgfile", DisplayName: "tgfile"}
	for _, upload := range page.Uploads {
		result.Uploads = append(result.Uploads, listedMultipartUploadItem{
			Key:               encodeListValue(upload.Key, encodingType),
			UploadID:          upload.UploadID,
			Initiator:         owner,
			Owner:             owner,
			StorageClass:      "STANDARD",
			Initiated:         formatS3Timestamp(upload.Initiated),
			ChecksumAlgorithm: string(upload.Algorithm),
			ChecksumType:      string(upload.ChecksumType),
		})
	}
	for _, prefix := range page.CommonPrefixes {
		result.CommonPrefixes = append(result.CommonPrefixes, commonPrefix{
			Prefix: encodeListValue(prefix, encodingType),
		})
	}
	return result
}

func validateMultipartQuery(
	query url.Values,
	required []string,
	optional []string,
) *s3base.APIError {
	declared := make(map[string]bool, len(required)+len(optional))
	for _, name := range required {
		declared[name] = true
		values, exists := query[name]
		if !exists || len(values) != 1 {
			return invalidMultipartArgument("A required multipart parameter is missing or repeated.", nil)
		}
	}
	for _, name := range optional {
		declared[name] = true
		if values, exists := query[name]; exists && len(values) != 1 {
			return invalidMultipartArgument("A multipart parameter was specified more than once.", nil)
		}
	}
	for name, values := range query {
		if declared[name] {
			continue
		}
		if name == "x-id" {
			if len(values) != 1 {
				return invalidMultipartArgument("x-id was specified more than once.", nil)
			}
			continue
		}
		if isSignatureQueryParameter(name) {
			continue
		}
		return invalidMultipartArgument("An unsupported multipart parameter was provided.", nil)
	}
	return nil
}

func isSignatureQueryParameter(name string) bool {
	switch strings.ToLower(name) {
	case "x-amz-algorithm", "x-amz-credential", "x-amz-date", "x-amz-expires",
		"x-amz-signedheaders", "x-amz-signature", "x-amz-security-token":
		return true
	default:
		return false
	}
}

func parseMultipartUploadID(value string) (string, *s3base.APIError) {
	if len(value) != multipartUploadIDLength {
		return "", invalidMultipartArgument("uploadId is invalid.", nil)
	}
	raw, err := hex.DecodeString(value)
	if err != nil || len(raw) != multipartUploadIDLength/2 || value != strings.ToLower(value) {
		return "", invalidMultipartArgument("uploadId is invalid.", err)
	}
	return value, nil
}

func parseBoundedMultipartInteger(
	value string,
	minimum, maximum int,
	name string,
) (int, *s3base.APIError) {
	if value == "" {
		return 0, invalidMultipartArgument(name+" is required.", nil)
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return 0, invalidMultipartArgument(name+" is invalid.", nil)
		}
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < minimum || parsed > maximum {
		return 0, invalidMultipartArgument(name+" is outside the supported range.", err)
	}
	return parsed, nil
}

func parseOptionalMultipartInteger(
	value string,
	minimum, maximum, defaultValue int,
	name string,
) (int, *s3base.APIError) {
	if value == "" {
		return defaultValue, nil
	}
	return parseBoundedMultipartInteger(value, minimum, maximum, name)
}

func rejectUnsupportedMultipartHeaders(request *http.Request) *s3base.APIError {
	if request.Header.Get("X-Amz-Copy-Source") != "" {
		return multipartNotImplemented("UploadPartCopy is not implemented.")
	}
	for name := range request.Header {
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "x-amz-server-side-encryption") ||
			lower == "x-amz-acl" ||
			strings.HasPrefix(lower, "x-amz-grant-") {
			return multipartNotImplemented("The requested multipart header is not implemented.")
		}
	}
	return nil
}

func multipartNotImplemented(message string) *s3base.APIError {
	return s3base.NewError(http.StatusNotImplemented, "NotImplemented", message, nil)
}

func invalidMultipartArgument(message string, cause error) *s3base.APIError {
	return s3base.NewError(http.StatusBadRequest, "InvalidArgument", message, cause)
}

func readMultipartCompleteBody(c *gin.Context) ([]byte, *s3base.APIError) {
	if c.Request.ContentLength > maxMultipartCompleteBody {
		return nil, malformedMultipartXML(errMultipartBodyTooLong)
	}
	reader := http.MaxBytesReader(c.Writer, c.Request.Body, maxMultipartCompleteBody)
	body, err := io.ReadAll(reader)
	if err != nil {
		var verifyError *s3verify.VerifyError
		if errors.As(err, &verifyError) {
			return nil, verifierBodyError(err)
		}
		return nil, malformedMultipartXML(errors.Join(errMultipartBodyTooLong, err))
	}
	if expectedRaw := c.GetHeader("Content-MD5"); expectedRaw != "" {
		expected, err := base64.StdEncoding.DecodeString(expectedRaw)
		if err != nil || len(expected) != filemgr.MD5CompatibilitySize {
			return nil, s3base.NewError(
				http.StatusBadRequest,
				"InvalidDigest",
				"The Content-MD5 value is invalid.",
				err,
			)
		}
		digest := filemgr.NewMD5CompatibilityHash()
		_, _ = digest.Write(body)
		if subtle.ConstantTimeCompare(digest.Sum(nil), expected) != 1 {
			return nil, s3base.NewError(
				http.StatusBadRequest,
				"BadDigest",
				"The Content-MD5 did not match.",
				errChecksumMismatch,
			)
		}
	}
	return body, nil
}

type decodedCompletePart struct {
	numberText        string
	etagText          string
	checksumText      string
	checksumAlgorithm s3checksum.Algorithm
	hasNumber         bool
	hasETag           bool
	hasChecksum       bool
}

type multipartCompleteXMLParser struct {
	stack        []xml.Name
	parts        []decodedCompletePart
	currentIndex int
	rootSeen     bool
}

func decodeMultipartCompleteBody(body []byte) ([]filemgr.CompleteMultipartPart, *s3base.APIError) {
	decoded, err := decodeMultipartCompleteXML(body)
	if err != nil {
		return nil, malformedMultipartXML(err)
	}
	if len(decoded) == 0 || len(decoded) > maxMultipartPartNumber {
		return nil, malformedMultipartXML(errMalformedCompleteXML)
	}
	parts := make([]filemgr.CompleteMultipartPart, 0, len(decoded))
	previous := 0
	for _, item := range decoded {
		number, apiError := parseBoundedMultipartInteger(
			strings.TrimSpace(item.numberText),
			1,
			maxMultipartPartNumber,
			"PartNumber",
		)
		if apiError != nil {
			return nil, malformedMultipartXML(apiError)
		}
		if number <= previous {
			return nil, multipartError(filemgr.ErrInvalidPartOrder)
		}
		etag, ok := normalizeMultipartPartETag(item.etagText)
		if !ok {
			return nil, multipartError(filemgr.ErrInvalidMultipartPart)
		}
		checksumValue := strings.TrimSpace(item.checksumText)
		if item.hasChecksum {
			if _, err := s3checksum.Decode(item.checksumAlgorithm, checksumValue); err != nil {
				return nil, invalidChecksumDigest(err)
			}
		}
		parts = append(parts, filemgr.CompleteMultipartPart{
			PartNumber:        number,
			ETag:              etag,
			ChecksumAlgorithm: item.checksumAlgorithm,
			ChecksumValue:     checksumValue,
		})
		previous = number
	}
	return parts, nil
}

func decodeMultipartCompleteXML(body []byte) ([]decodedCompletePart, error) {
	decoder := xml.NewDecoder(bytes.NewReader(body))
	decoder.Strict = true
	parser := multipartCompleteXMLParser{
		stack:        make([]xml.Name, 0, 3),
		parts:        make([]decodedCompletePart, 0),
		currentIndex: -1,
	}
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: %w", errMalformedCompleteXML, err)
		}
		if err := parser.consume(token); err != nil {
			return nil, err
		}
	}
	if len(parser.stack) != 0 || !parser.rootSeen || len(parser.parts) == 0 {
		return nil, errMalformedCompleteXML
	}
	return parser.parts, nil
}

func (p *multipartCompleteXMLParser) consume(token xml.Token) error {
	switch value := token.(type) {
	case xml.StartElement:
		return p.start(value)
	case xml.EndElement:
		return p.end(value)
	case xml.CharData:
		return p.characterData(value)
	case xml.ProcInst:
		if value.Target != "xml" || len(p.stack) != 0 {
			return errMalformedCompleteXML
		}
		return nil
	default:
		return errMalformedCompleteXML
	}
}

func (p *multipartCompleteXMLParser) start(element xml.StartElement) error {
	if element.Name.Space != "" && element.Name.Space != s3XMLNamespace {
		return errMalformedCompleteXML
	}
	depth := len(p.stack)
	var err error
	switch depth {
	case 0:
		err = p.startRoot(element)
	case 1:
		err = p.startPart(element)
	case 2:
		err = p.startPartField(element)
	default:
		return errMalformedCompleteXML
	}
	if err != nil {
		return err
	}
	p.stack = append(p.stack, element.Name)
	return nil
}

func (p *multipartCompleteXMLParser) startRoot(element xml.StartElement) error {
	if element.Name.Local != "CompleteMultipartUpload" ||
		p.rootSeen ||
		!validMultipartRootAttributes(element.Attr) {
		return errMalformedCompleteXML
	}
	p.rootSeen = true
	return nil
}

func (p *multipartCompleteXMLParser) startPart(element xml.StartElement) error {
	if element.Name.Local != "Part" || len(element.Attr) != 0 {
		return errMalformedCompleteXML
	}
	p.parts = append(p.parts, decodedCompletePart{})
	p.currentIndex = len(p.parts) - 1
	return nil
}

func (p *multipartCompleteXMLParser) startPartField(element xml.StartElement) error {
	if p.currentIndex < 0 || len(element.Attr) != 0 {
		return errMalformedCompleteXML
	}
	current := &p.parts[p.currentIndex]
	switch element.Name.Local {
	case "PartNumber":
		if current.hasNumber {
			return errMalformedCompleteXML
		}
		current.hasNumber = true
	case "ETag":
		if current.hasETag {
			return errMalformedCompleteXML
		}
		current.hasETag = true
	case "ChecksumCRC32", "ChecksumCRC32C", "ChecksumCRC64NVME", "ChecksumSHA1", "ChecksumSHA256":
		if current.hasChecksum {
			return errMalformedCompleteXML
		}
		algorithm, err := s3checksum.ParseAlgorithm(strings.TrimPrefix(element.Name.Local, "Checksum"))
		if err != nil {
			return errMalformedCompleteXML
		}
		current.hasChecksum = true
		current.checksumAlgorithm = algorithm
	default:
		return errMalformedCompleteXML
	}
	return nil
}

func (p *multipartCompleteXMLParser) end(element xml.EndElement) error {
	if len(p.stack) == 0 || p.stack[len(p.stack)-1] != element.Name {
		return errMalformedCompleteXML
	}
	if len(p.stack) == 2 {
		if p.currentIndex < 0 ||
			!p.parts[p.currentIndex].hasNumber ||
			!p.parts[p.currentIndex].hasETag {
			return errMalformedCompleteXML
		}
		p.currentIndex = -1
	}
	p.stack = p.stack[:len(p.stack)-1]
	return nil
}

func (p *multipartCompleteXMLParser) characterData(data xml.CharData) error {
	if len(p.stack) != 3 || p.currentIndex < 0 {
		if strings.TrimSpace(string(data)) != "" {
			return errMalformedCompleteXML
		}
		return nil
	}
	switch p.stack[2].Local {
	case "PartNumber":
		p.parts[p.currentIndex].numberText += string(data)
	case "ETag":
		p.parts[p.currentIndex].etagText += string(data)
	case "ChecksumCRC32", "ChecksumCRC32C", "ChecksumCRC64NVME", "ChecksumSHA1", "ChecksumSHA256":
		p.parts[p.currentIndex].checksumText += string(data)
	default:
		return errMalformedCompleteXML
	}
	return nil
}

func validMultipartRootAttributes(attributes []xml.Attr) bool {
	for _, attribute := range attributes {
		isDefaultNamespace := attribute.Name.Space == "" && attribute.Name.Local == "xmlns"
		isNamespaceDeclaration := attribute.Name.Space == "xmlns"
		if (!isDefaultNamespace && !isNamespaceDeclaration) || attribute.Value != s3XMLNamespace {
			return false
		}
	}
	return true
}

func normalizeMultipartPartETag(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "W/") || strings.Contains(value, ",") {
		return "", false
	}
	if strings.HasPrefix(value, `"`) || strings.HasSuffix(value, `"`) {
		if len(value) < 2 || !strings.HasPrefix(value, `"`) || !strings.HasSuffix(value, `"`) {
			return "", false
		}
		value = value[1 : len(value)-1]
	}
	value = strings.ToLower(value)
	raw, err := hex.DecodeString(value)
	return value, err == nil && len(raw) == filemgr.MD5CompatibilitySize
}

func malformedMultipartXML(cause error) *s3base.APIError {
	return s3base.NewError(
		http.StatusBadRequest,
		"MalformedXML",
		"The XML you provided was not well-formed or did not validate.",
		cause,
	)
}

func multipartError(err error) *s3base.APIError {
	switch {
	case errors.Is(err, filemgr.ErrNoSuchUpload):
		return s3base.NewError(http.StatusNotFound, "NoSuchUpload", "The specified upload does not exist.", err)
	case errors.Is(err, filemgr.ErrInvalidMultipartPart):
		return s3base.NewError(
			http.StatusBadRequest,
			"InvalidPart",
			"One or more of the specified parts could not be found.",
			err,
		)
	case errors.Is(err, filemgr.ErrInvalidPartOrder):
		return s3base.NewError(
			http.StatusBadRequest,
			"InvalidPartOrder",
			"The list of parts was not in ascending order.",
			err,
		)
	case errors.Is(err, filemgr.ErrMultipartPartTooSmall):
		return s3base.NewError(
			http.StatusBadRequest,
			"EntityTooSmall",
			"Your proposed upload is smaller than the minimum allowed size.",
			err,
		)
	case errors.Is(err, filemgr.ErrMultipartEntityTooLarge), errors.Is(err, filemgr.ErrTooManyFileParts):
		return s3base.NewError(
			http.StatusBadRequest,
			"EntityTooLarge",
			"Your proposed upload exceeds the maximum allowed size.",
			err,
		)
	case errors.Is(err, filemgr.ErrInvalidMultipartRequest):
		return invalidMultipartArgument("A multipart request parameter is invalid.", err)
	case errors.Is(err, filemgr.ErrMultipartChecksum),
		errors.Is(err, filemgr.ErrMultipartChecksumType):
		return s3base.NewError(
			http.StatusBadRequest,
			"BadDigest",
			"The multipart checksum did not match.",
			err,
		)
	case errors.Is(err, filemgr.ErrMultipartObjectSize):
		return s3base.InvalidRequest("The multipart object size did not match.", err)
	case errors.Is(err, filemgr.ErrS3Precondition),
		errors.Is(err, filemgr.ErrS3ObjectConflict),
		errors.Is(err, filemgr.ErrMultipartConflict):
		return mutationError(err)
	default:
		return s3base.InternalError(err)
	}
}

func setChecksumXML(
	fields *checksumXMLFields,
	algorithm s3checksum.Algorithm,
	value string,
) {
	if fields == nil || value == "" {
		return
	}
	switch algorithm {
	case s3checksum.AlgorithmCRC32:
		fields.ChecksumCRC32 = value
	case s3checksum.AlgorithmCRC32C:
		fields.ChecksumCRC32C = value
	case s3checksum.AlgorithmCRC64NVME:
		fields.ChecksumCRC64NVME = value
	case s3checksum.AlgorithmSHA1:
		fields.ChecksumSHA1 = value
	case s3checksum.AlgorithmSHA256:
		fields.ChecksumSHA256 = value
	}
}

func formatS3Timestamp(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}
