package s3

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/xxxsen/tgfile/filemgr"
	"github.com/xxxsen/tgfile/server/handler/s3/s3base"

	"github.com/gin-gonic/gin"
)

var (
	errContinuationTokenSize     = errors.New("invalid continuation token size")
	errContinuationTokenEncoding = errors.New("invalid continuation token encoding")
	errContinuationTokenTrailing = errors.New("invalid trailing continuation token data")
)

const (
	defaultListMaxKeys         = 1000
	maxContinuationTokenLength = 4096
)

type listAllBucketsResult struct {
	XMLName xml.Name         `xml:"ListAllMyBucketsResult"`
	XMLNS   string           `xml:"xmlns,attr"`
	Owner   listOwner        `xml:"Owner"`
	Buckets bucketCollection `xml:"Buckets"`
}

type listOwner struct {
	ID          string `xml:"ID"`
	DisplayName string `xml:"DisplayName"`
}

type bucketCollection struct {
	Buckets []listedBucket `xml:"Bucket"`
}

type listedBucket struct {
	Name         string `xml:"Name"`
	CreationDate string `xml:"CreationDate"`
}

func (h *S3Handler) ListBuckets(c *gin.Context) {
	if _, apiError := h.Authorize(c, true); apiError != nil {
		s3base.WriteError(c, apiError)
		return
	}
	names := make([]string, 0, len(h.bucketList))
	for _, bucket := range h.bucketList {
		names = append(names, bucket.Name)
	}
	slices.Sort(names)
	buckets := make([]listedBucket, 0, len(names))
	for _, name := range names {
		buckets = append(buckets, listedBucket{
			Name:         name,
			CreationDate: "1970-01-01T00:00:00.000Z",
		})
	}
	c.XML(http.StatusOK, &listAllBucketsResult{
		XMLNS:   "http://s3.amazonaws.com/doc/2006-03-01/",
		Owner:   listOwner{ID: "tgfile", DisplayName: "tgfile"},
		Buckets: bucketCollection{Buckets: buckets},
	})
}

func (h *S3Handler) GetBucketOrObject(c *gin.Context) {
	_, key := requestBucketKey(c.Request.URL.Path)
	if key == "" {
		h.GetBucket(c)
		return
	}
	if hasQueryKey(c.Request.URL.Query(), "uploadId") {
		h.ListParts(c)
		return
	}
	h.DownloadObject(c)
}

func (h *S3Handler) GetBucket(c *gin.Context) {
	bucketName, _ := requestBucketKey(c.Request.URL.Path)
	if _, exists := h.Bucket(bucketName); !exists {
		s3base.WriteError(c, noSuchBucketError(bucketName))
		return
	}
	if _, apiError := h.Authorize(c, true); apiError != nil {
		s3base.WriteError(c, apiError)
		return
	}
	query := c.Request.URL.Query()
	switch {
	case query.Get("list-type") == "2":
		h.listObjectsV2(c, bucketName)
	case hasQueryKey(query, "location"):
		s3base.SimpleReply(c)
	case hasQueryKey(query, "uploads"):
		h.ListMultipartUploads(c)
	case hasUnsupportedBucketSubresource(query):
		writeUnsupportedBucketSubresource(c)
	case isListObjectsV1Request(c.Request):
		h.listObjectsV1(c, bucketName)
	case c.Request.URL.RawQuery == "":
		s3base.SimpleReply(c)
	default:
		writeUnsupportedBucketSubresource(c)
	}
}

func (h *S3Handler) HeadBucketOrObject(c *gin.Context) {
	_, key := requestBucketKey(c.Request.URL.Path)
	if key == "" {
		h.HeadBucket(c)
		return
	}
	h.HeadObject(c)
}

func (h *S3Handler) PostBucketOrObject(c *gin.Context) {
	_, key := requestBucketKey(c.Request.URL.Path)
	if key == "" {
		h.DeleteObjects(c)
		return
	}
	query := c.Request.URL.Query()
	hasUploads := hasQueryKey(query, "uploads")
	hasUploadID := hasQueryKey(query, "uploadId")
	switch {
	case hasUploads && hasUploadID:
		s3base.WriteError(c, s3base.InvalidRequest("uploads and uploadId cannot be combined.", nil))
	case hasUploads:
		h.CreateMultipartUpload(c)
	case hasUploadID:
		h.CompleteMultipartUpload(c)
	default:
		h.NotImplemented(c)
	}
}

func (h *S3Handler) HeadBucket(c *gin.Context) {
	bucketName, _ := requestBucketKey(c.Request.URL.Path)
	if _, exists := h.Bucket(bucketName); !exists {
		s3base.WriteError(c, noSuchBucketError(bucketName))
		return
	}
	if _, apiError := h.Authorize(c, true); apiError != nil {
		s3base.WriteError(c, apiError)
		return
	}
	c.Status(http.StatusOK)
}

func hasQueryKey(query url.Values, key string) bool {
	_, exists := query[key]
	return exists
}

func isListObjectsV1Request(request *http.Request) bool {
	query := request.URL.Query()
	if hasQueryKey(query, "list-type") {
		return false
	}
	for _, key := range []string{"prefix", "delimiter", "marker", "max-keys", "encoding-type"} {
		if hasQueryKey(query, key) {
			return true
		}
	}
	return strings.HasSuffix(request.URL.Path, "/")
}

func hasUnsupportedBucketSubresource(query url.Values) bool {
	for key := range query {
		switch strings.ToLower(key) {
		case "accelerate", "acl", "analytics", "cors", "delete", "encryption",
			"inventory", "lifecycle", "logging", "metrics", "notification",
			"object-lock", "ownershipcontrols", "policy", "publicaccessblock",
			"replication", "requestpayment", "tagging", "uploads", "versioning",
			"website":
			return true
		}
	}
	return false
}

func writeUnsupportedBucketSubresource(c *gin.Context) {
	s3base.WriteError(c, s3base.NewError(
		http.StatusNotImplemented,
		"NotImplemented",
		"The requested bucket subresource is not implemented.",
		nil,
	))
}

type listBucketV2Result struct {
	XMLName               xml.Name       `xml:"ListBucketResult"`
	XMLNS                 string         `xml:"xmlns,attr"`
	Name                  string         `xml:"Name"`
	Prefix                string         `xml:"Prefix"`
	Delimiter             string         `xml:"Delimiter,omitempty"`
	MaxKeys               int            `xml:"MaxKeys"`
	KeyCount              int            `xml:"KeyCount"`
	IsTruncated           bool           `xml:"IsTruncated"`
	ContinuationToken     string         `xml:"ContinuationToken,omitempty"`
	NextContinuationToken string         `xml:"NextContinuationToken,omitempty"`
	StartAfter            string         `xml:"StartAfter,omitempty"`
	EncodingType          string         `xml:"EncodingType,omitempty"`
	Contents              []listContent  `xml:"Contents,omitempty"`
	CommonPrefixes        []commonPrefix `xml:"CommonPrefixes,omitempty"`
}

type listBucketV1Result struct {
	XMLName        xml.Name       `xml:"ListBucketResult"`
	XMLNS          string         `xml:"xmlns,attr"`
	Name           string         `xml:"Name"`
	Prefix         string         `xml:"Prefix"`
	Marker         string         `xml:"Marker"`
	NextMarker     string         `xml:"NextMarker,omitempty"`
	MaxKeys        int            `xml:"MaxKeys"`
	Delimiter      string         `xml:"Delimiter,omitempty"`
	IsTruncated    bool           `xml:"IsTruncated"`
	EncodingType   string         `xml:"EncodingType,omitempty"`
	Contents       []listContent  `xml:"Contents,omitempty"`
	CommonPrefixes []commonPrefix `xml:"CommonPrefixes,omitempty"`
}

type listContent struct {
	Key          string     `xml:"Key"`
	LastModified string     `xml:"LastModified"`
	ETag         string     `xml:"ETag"`
	Size         int64      `xml:"Size"`
	StorageClass string     `xml:"StorageClass"`
	Owner        *listOwner `xml:"Owner,omitempty"`
}

type commonPrefix struct {
	Prefix string `xml:"Prefix"`
}

type continuationToken struct {
	Version   int    `json:"v"`
	Bucket    string `json:"bucket"`
	Prefix    string `json:"prefix"`
	Delimiter string `json:"delimiter"`
	LastItem  string `json:"last_item"`
}

func (h *S3Handler) listObjectsV1(c *gin.Context, bucket string) {
	query := c.Request.URL.Query()
	request, marker, encodingType, apiError := parseListV1Request(query, bucket)
	if apiError != nil {
		s3base.WriteError(c, apiError)
		return
	}
	result, err := h.fmgr.ListS3Objects(c.Request.Context(), request)
	if err != nil {
		s3base.WriteError(c, s3base.InternalError(err))
		return
	}
	response := &listBucketV1Result{
		XMLNS:          "http://s3.amazonaws.com/doc/2006-03-01/",
		Name:           bucket,
		Prefix:         encodeListValue(request.Prefix, encodingType),
		Marker:         encodeListValue(marker, encodingType),
		MaxKeys:        request.MaxKeys,
		Delimiter:      encodeListValue(request.Delimiter, encodingType),
		IsTruncated:    result.IsTruncated,
		EncodingType:   encodingType,
		Contents:       listContents(result.Items, encodingType, true),
		CommonPrefixes: listCommonPrefixes(result.CommonPrefixes, encodingType),
	}
	if result.IsTruncated && request.Delimiter != "" {
		response.NextMarker = encodeListValue(result.NextKey, encodingType)
	}
	c.XML(http.StatusOK, response)
}

func (h *S3Handler) listObjectsV2(c *gin.Context, bucket string) {
	query := c.Request.URL.Query()
	request, tokenText, encodingType, apiError := parseListV2Request(query, bucket)
	if apiError != nil {
		s3base.WriteError(c, apiError)
		return
	}
	result, err := h.fmgr.ListS3Objects(c.Request.Context(), request)
	if err != nil {
		s3base.WriteError(c, s3base.InternalError(err))
		return
	}
	response := &listBucketV2Result{
		XMLNS:             "http://s3.amazonaws.com/doc/2006-03-01/",
		Name:              bucket,
		Prefix:            encodeListValue(request.Prefix, encodingType),
		Delimiter:         encodeListValue(request.Delimiter, encodingType),
		MaxKeys:           request.MaxKeys,
		KeyCount:          len(result.Items) + len(result.CommonPrefixes),
		IsTruncated:       result.IsTruncated,
		ContinuationToken: tokenText,
		StartAfter:        encodeListValue(request.StartAfter, encodingType),
		EncodingType:      encodingType,
		Contents:          listContents(result.Items, encodingType, request.FetchOwner),
		CommonPrefixes:    listCommonPrefixes(result.CommonPrefixes, encodingType),
	}
	if result.IsTruncated {
		response.NextContinuationToken, err = encodeContinuationToken(continuationToken{
			Version:   1,
			Bucket:    bucket,
			Prefix:    request.Prefix,
			Delimiter: request.Delimiter,
			LastItem:  result.NextKey,
		})
		if err != nil {
			s3base.WriteError(c, s3base.InternalError(err))
			return
		}
	}
	c.XML(http.StatusOK, response)
}

func listContents(items []filemgr.S3ListItem, encodingType string, fetchOwner bool) []listContent {
	contents := make([]listContent, 0, len(items))
	for _, item := range items {
		content := listContent{
			Key:          encodeListValue(item.Key, encodingType),
			LastModified: time.UnixMilli(item.LastModified).UTC().Format("2006-01-02T15:04:05.000Z"),
			ETag:         item.ETag,
			Size:         item.Size,
			StorageClass: "STANDARD",
		}
		if fetchOwner {
			content.Owner = &listOwner{ID: "tgfile", DisplayName: "tgfile"}
		}
		contents = append(contents, content)
	}
	return contents
}

func listCommonPrefixes(prefixes []string, encodingType string) []commonPrefix {
	result := make([]commonPrefix, 0, len(prefixes))
	for _, prefix := range prefixes {
		result = append(result, commonPrefix{
			Prefix: encodeListValue(prefix, encodingType),
		})
	}
	return result
}

func parseListV1Request(
	query url.Values,
	bucket string,
) (*filemgr.S3ListRequest, string, string, *s3base.APIError) {
	if apiError := validateListV1Singletons(query); apiError != nil {
		return nil, "", "", apiError
	}
	delimiter, encodingType, apiError := parseListOptions(query)
	if apiError != nil {
		return nil, "", "", apiError
	}
	maxKeys, apiError := parseListMaxKeys(query.Get("max-keys"))
	if apiError != nil {
		return nil, "", "", apiError
	}
	marker := query.Get("marker")
	return &filemgr.S3ListRequest{
		Bucket:     bucket,
		Prefix:     query.Get("prefix"),
		Delimiter:  delimiter,
		StartAfter: marker,
		MaxKeys:    maxKeys,
		FetchOwner: true,
	}, marker, encodingType, nil
}

func parseListV2Request(
	query url.Values,
	bucket string,
) (*filemgr.S3ListRequest, string, string, *s3base.APIError) {
	if apiError := validateListV2Singletons(query); apiError != nil {
		return nil, "", "", apiError
	}
	if query.Get("list-type") != "2" {
		return nil, "", "", s3base.InvalidRequest("list-type must be 2.", nil)
	}
	delimiter, encodingType, apiError := parseListOptions(query)
	if apiError != nil {
		return nil, "", "", apiError
	}
	maxKeys, apiError := parseListMaxKeys(query.Get("max-keys"))
	if apiError != nil {
		return nil, "", "", apiError
	}
	startAfter := query.Get("start-after")
	tokenText := query.Get("continuation-token")
	if startAfter != "" && tokenText != "" {
		return nil, "", "", s3base.InvalidRequest(
			"continuation-token and start-after cannot be combined.",
			nil,
		)
	}
	request := &filemgr.S3ListRequest{
		Bucket:     bucket,
		Prefix:     query.Get("prefix"),
		Delimiter:  delimiter,
		StartAfter: startAfter,
		MaxKeys:    maxKeys,
		FetchOwner: query.Get("fetch-owner") == "true",
	}
	if tokenText != "" {
		token, err := decodeContinuationToken(tokenText)
		if err != nil || !continuationTokenMatches(token, request) {
			return nil, "", "", s3base.NewError(
				http.StatusBadRequest,
				"InvalidToken",
				"The continuation token is invalid.",
				err,
			)
		}
		request.ContinuationToken = token.LastItem
	}
	return request, tokenText, encodingType, nil
}

func validateListV1Singletons(query url.Values) *s3base.APIError {
	return validateListSingletons(query, []string{
		"prefix", "delimiter", "marker", "max-keys", "encoding-type",
	})
}

func validateListV2Singletons(query url.Values) *s3base.APIError {
	return validateListSingletons(query, []string{
		"list-type", "prefix", "delimiter", "max-keys", "start-after",
		"continuation-token", "encoding-type", "fetch-owner",
	})
}

func validateListSingletons(query url.Values, singletons []string) *s3base.APIError {
	for _, singleton := range singletons {
		if len(query[singleton]) > 1 {
			return s3base.NewError(
				http.StatusBadRequest,
				"InvalidArgument",
				"A list parameter was specified more than once.",
				nil,
			)
		}
	}
	return nil
}

func parseListOptions(query url.Values) (string, string, *s3base.APIError) {
	delimiter := query.Get("delimiter")
	if delimiter != "" && delimiter != "/" {
		return "", "", s3base.NewError(
			http.StatusBadRequest,
			"InvalidArgument",
			"Only an empty delimiter or / is supported.",
			nil,
		)
	}
	encodingType := query.Get("encoding-type")
	if encodingType != "" && encodingType != "url" {
		return "", "", s3base.NewError(
			http.StatusBadRequest,
			"InvalidArgument",
			"encoding-type must be url when provided.",
			nil,
		)
	}
	fetchOwner := query.Get("fetch-owner")
	if fetchOwner != "" && fetchOwner != "true" && fetchOwner != "false" {
		return "", "", s3base.NewError(
			http.StatusBadRequest,
			"InvalidArgument",
			"fetch-owner must be true or false.",
			nil,
		)
	}
	return delimiter, encodingType, nil
}

func parseListMaxKeys(raw string) (int, *s3base.APIError) {
	if raw == "" {
		return defaultListMaxKeys, nil
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed < 0 || parsed > defaultListMaxKeys {
		return 0, s3base.NewError(
			http.StatusBadRequest,
			"InvalidArgument",
			"max-keys must be between 0 and 1000.",
			err,
		)
	}
	return parsed, nil
}

func continuationTokenMatches(token *continuationToken, request *filemgr.S3ListRequest) bool {
	return token != nil &&
		token.Version == 1 &&
		token.Bucket == request.Bucket &&
		token.Prefix == request.Prefix &&
		token.Delimiter == request.Delimiter &&
		token.LastItem != ""
}

func encodeContinuationToken(token continuationToken) (string, error) {
	raw, err := json.Marshal(token)
	if err != nil {
		return "", fmt.Errorf("encode continuation token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeContinuationToken(value string) (*continuationToken, error) {
	if value == "" || len(value) > maxContinuationTokenLength {
		return nil, errContinuationTokenSize
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(raw) > maxContinuationTokenLength {
		return nil, errors.Join(errContinuationTokenEncoding, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var token continuationToken
	if err := decoder.Decode(&token); err != nil {
		return nil, fmt.Errorf("decode continuation token: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return nil, errContinuationTokenTrailing
	}
	return &token, nil
}

func encodeListValue(value, encodingType string) string {
	if encodingType != "url" {
		return value
	}
	return strings.ReplaceAll(url.PathEscape(value), "%2F", "/")
}

func noSuchBucketError(bucket string) *s3base.APIError {
	apiError := s3base.NewError(
		http.StatusNotFound,
		"NoSuchBucket",
		"The specified bucket does not exist.",
		nil,
	)
	apiError.Bucket = bucket
	return apiError
}

func (h *S3Handler) NotImplemented(c *gin.Context) {
	if _, apiError := h.Authorize(c, true); apiError != nil {
		s3base.WriteError(c, apiError)
		return
	}
	s3base.WriteError(c, s3base.NewError(
		http.StatusNotImplemented,
		"NotImplemented",
		"The requested operation is not implemented.",
		nil,
	))
}
