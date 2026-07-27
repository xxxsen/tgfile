package s3

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/xxxsen/tgfile/authz"
	"github.com/xxxsen/tgfile/filemgr"
	"github.com/xxxsen/tgfile/server/handler/s3/s3base"

	"github.com/gin-gonic/gin"
	"github.com/xxxsen/s3verify"
)

var (
	errUnexpectedDeleteXMLElement = errors.New("unexpected delete XML element")
	errUnbalancedDeleteXML        = errors.New("unbalanced delete XML")
	errUnclosedDeleteXML          = errors.New("unclosed delete XML element")
)

const (
	maxDeleteObjects     = 1000
	maxDeleteRequestBody = 2 * 1024 * 1024
	s3XMLNamespace       = "http://s3.amazonaws.com/doc/2006-03-01/"
)

func (h *S3Handler) DeleteObject(c *gin.Context) {
	if hasQueryKey(c.Request.URL.Query(), "uploadId") {
		h.AbortMultipartUpload(c)
		return
	}
	if h.rejectUnsupportedObjectQuery(c) {
		return
	}
	bucket, key, apiError := h.authorizeWrite(c)
	if apiError != nil {
		s3base.WriteError(c, apiError)
		return
	}
	if err := validateHistoricalObjectKeyBoundary(bucket.Name, key); err != nil {
		s3base.WriteError(c, objectNameError(err))
		return
	}
	objectPath := "/" + bucket.Name + "/" + key
	condition, apiError := parseDeleteCondition(c.Request)
	if apiError != nil {
		s3base.WriteError(c, apiError)
		return
	}
	unlock := h.locks.lock(objectPath)
	defer unlock()
	if err := validateExistingOrHistoricalKey(c.Request.Context(), h.fmgr, objectPath, key); err != nil {
		s3base.WriteError(c, objectNameError(err))
		return
	}
	_, err := h.fmgr.DeleteS3Object(c.Request.Context(), objectPath, condition)
	if err != nil {
		s3base.WriteError(c, mutationError(err))
		return
	}
	c.Status(http.StatusNoContent)
}

func parseDeleteCondition(request *http.Request) (*filemgr.S3Condition, *s3base.APIError) {
	ifMatch := request.Header.Get("If-Match")
	if ifMatch != "" && ifMatch != "*" && !validSingleETag(ifMatch) {
		return nil, s3base.InvalidRequest("If-Match must contain one valid ETag.", nil)
	}
	return &filemgr.S3Condition{IfMatch: ifMatch}, nil
}

func validateExistingOrHistoricalKey(
	ctx context.Context,
	manager filemgr.IFileManager,
	objectPath, key string,
) error {
	if validateNewObjectKey(key) == nil {
		return nil
	}
	if _, err := manager.StatS3Object(ctx, objectPath); err == nil {
		return nil
	}
	return errInvalidObjectName
}

type deleteObjectsRequest struct {
	XMLName xml.Name              `xml:"Delete"`
	Objects []deleteObjectRequest `xml:"Object"`
	Quiet   bool                  `xml:"Quiet"`
}

type deleteObjectRequest struct {
	Key  string `xml:"Key"`
	ETag string `xml:"ETag,omitempty"`
}

type deleteObjectsResult struct {
	XMLName xml.Name            `xml:"DeleteResult"`
	XMLNS   string              `xml:"xmlns,attr"`
	Deleted []deletedObject     `xml:"Deleted,omitempty"`
	Errors  []deleteObjectError `xml:"Error,omitempty"`
}

type deletedObject struct {
	Key string `xml:"Key"`
}

type deleteObjectError struct {
	Key     string `xml:"Key"`
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

func (h *S3Handler) DeleteObjects(c *gin.Context) {
	bucketName, _ := requestBucketKey(c.Request.URL.Path)
	bucket, exists := h.Bucket(bucketName)
	if !exists {
		s3base.WriteError(c, s3base.NewError(
			http.StatusNotFound,
			"NoSuchBucket",
			"The specified bucket does not exist.",
			nil,
		))
		return
	}
	if _, apiError := h.Authorize(c, true, authz.S3Write); apiError != nil {
		s3base.WriteError(c, apiError)
		return
	}
	if !validDeleteSubresource(c.Request.URL.Query()) {
		s3base.WriteError(c, s3base.NewError(
			http.StatusNotImplemented,
			"NotImplemented",
			"The requested bucket operation is not implemented.",
			nil,
		))
		return
	}
	if c.Request.ContentLength > maxDeleteRequestBody {
		s3base.WriteError(c, s3base.NewError(
			http.StatusBadRequest,
			"MalformedXML",
			"The XML request body is too large.",
			nil,
		))
		return
	}
	body, apiError := readDeleteBody(c)
	if apiError != nil {
		s3base.WriteError(c, apiError)
		return
	}
	request, apiError := decodeDeleteObjects(body)
	if apiError != nil {
		s3base.WriteError(c, apiError)
		return
	}
	result := &deleteObjectsResult{
		Deleted: make([]deletedObject, 0, len(request.Objects)),
		Errors:  make([]deleteObjectError, 0),
		XMLNS:   s3XMLNamespace,
	}
	for _, object := range request.Objects {
		itemError := h.deleteObjectItem(c, bucket, object)
		if itemError != nil {
			result.Errors = append(result.Errors, deleteObjectError{
				Key:     object.Key,
				Code:    itemError.Code,
				Message: itemError.Message,
			})
		} else if !request.Quiet {
			result.Deleted = append(result.Deleted, deletedObject{Key: object.Key})
		}
	}
	c.XML(http.StatusOK, result)
}

func validDeleteSubresource(query map[string][]string) bool {
	deleteValues, exists := query["delete"]
	if !exists || len(deleteValues) != 1 || deleteValues[0] != "" {
		return false
	}
	for key := range query {
		if key == "delete" {
			continue
		}
		switch strings.ToLower(key) {
		case "x-amz-algorithm", "x-amz-credential", "x-amz-date", "x-amz-expires",
			"x-amz-signedheaders", "x-amz-signature", "x-amz-security-token":
		default:
			return false
		}
	}
	return true
}

func readDeleteBody(c *gin.Context) ([]byte, *s3base.APIError) {
	hashes, reader, apiError := newUploadHashes(c.Request)
	if apiError != nil {
		return nil, apiError
	}
	if hashes.contentMD5 == "" && hashes.request == nil {
		return nil, s3base.NewError(
			http.StatusBadRequest,
			"InvalidDigest",
			"Content-MD5 or an additional checksum is required.",
			nil,
		)
	}
	body, err := io.ReadAll(io.LimitReader(reader, maxDeleteRequestBody+1))
	if err != nil {
		var verifyError *s3verify.VerifyError
		if errors.As(err, &verifyError) {
			return nil, verifierBodyError(err)
		}
		return nil, s3base.NewError(http.StatusBadRequest, "MalformedXML", "The XML body is invalid.", err)
	}
	if len(body) > maxDeleteRequestBody {
		return nil, s3base.NewError(
			http.StatusBadRequest,
			"MalformedXML",
			"The XML request body is too large.",
			nil,
		)
	}
	if apiError := hashes.loadTrailer(c); apiError != nil {
		return nil, apiError
	}
	if apiError := hashes.validate(); apiError != nil {
		return nil, apiError
	}
	return body, nil
}

func decodeDeleteObjects(body []byte) (*deleteObjectsRequest, *s3base.APIError) {
	if err := validateDeleteXMLStructure(body); err != nil {
		return nil, s3base.NewError(http.StatusBadRequest, "MalformedXML", "The XML body is invalid.", err)
	}
	var request deleteObjectsRequest
	decoder := xml.NewDecoder(bytes.NewReader(body))
	decoder.Strict = true
	if err := decoder.Decode(&request); err != nil || request.XMLName.Local != "Delete" {
		return nil, s3base.NewError(http.StatusBadRequest, "MalformedXML", "The XML body is invalid.", err)
	}
	if len(request.Objects) == 0 || len(request.Objects) > maxDeleteObjects {
		return nil, s3base.InvalidRequest("DeleteObjects requires between 1 and 1000 objects.", nil)
	}
	for _, object := range request.Objects {
		if object.Key == "" {
			return nil, s3base.InvalidRequest("Every delete object must contain a Key.", nil)
		}
		if object.ETag != "" && !validSingleETag(object.ETag) {
			return nil, s3base.InvalidRequest("DeleteObjects ETag is invalid.", nil)
		}
	}
	return &request, nil
}

func validateDeleteXMLStructure(body []byte) error {
	decoder := xml.NewDecoder(bytes.NewReader(body))
	state := deleteXMLValidationState{stack: make([]string, 0, 3)}
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read delete XML token: %w", err)
		}
		switch value := token.(type) {
		case xml.StartElement:
			if err := state.start(value); err != nil {
				return err
			}
		case xml.EndElement:
			if err := state.end(value); err != nil {
				return err
			}
		case xml.CharData:
			if err := state.characterData(value); err != nil {
				return err
			}
		}
	}
	if len(state.stack) != 0 || !state.rootSeen {
		return errUnclosedDeleteXML
	}
	return nil
}

type deleteXMLValidationState struct {
	stack           []string
	rootSeen        bool
	quietCount      int
	objectKeyCount  int
	objectETagCount int
}

func (s *deleteXMLValidationState) start(element xml.StartElement) error {
	parent := s.parent()
	if parent == "" {
		if s.rootSeen {
			return fmt.Errorf("%w: multiple root elements", errUnexpectedDeleteXMLElement)
		}
		s.rootSeen = true
	}
	if !allowedDeleteElement(parent, element.Name.Local) ||
		!allowedDeleteNamespace(element.Name.Space) ||
		!allowedDeleteAttributes(parent, element) {
		return fmt.Errorf("%w: %q", errUnexpectedDeleteXMLElement, element.Name.Local)
	}
	if err := s.trackElementCount(parent, element.Name.Local); err != nil {
		return err
	}
	s.stack = append(s.stack, element.Name.Local)
	return nil
}

func (s *deleteXMLValidationState) trackElementCount(parent, element string) error {
	switch {
	case parent == "Delete" && element == "Object":
		s.objectKeyCount = 0
		s.objectETagCount = 0
	case parent == "Delete" && element == "Quiet":
		s.quietCount++
		if s.quietCount > 1 {
			return fmt.Errorf("%w: duplicate Quiet", errUnexpectedDeleteXMLElement)
		}
	case parent == "Object" && element == "Key":
		s.objectKeyCount++
		if s.objectKeyCount > 1 {
			return fmt.Errorf("%w: duplicate Key", errUnexpectedDeleteXMLElement)
		}
	case parent == "Object" && element == "ETag":
		s.objectETagCount++
		if s.objectETagCount > 1 {
			return fmt.Errorf("%w: duplicate ETag", errUnexpectedDeleteXMLElement)
		}
	}
	return nil
}

func (s *deleteXMLValidationState) end(element xml.EndElement) error {
	if len(s.stack) == 0 || s.stack[len(s.stack)-1] != element.Name.Local {
		return errUnbalancedDeleteXML
	}
	if element.Name.Local == "Object" && s.objectKeyCount != 1 {
		return fmt.Errorf("%w: Object must contain one Key", errUnexpectedDeleteXMLElement)
	}
	s.stack = s.stack[:len(s.stack)-1]
	return nil
}

func (s *deleteXMLValidationState) characterData(value xml.CharData) error {
	parent := s.parent()
	if len(bytes.TrimSpace(value)) != 0 &&
		(parent == "" || parent == "Delete" || parent == "Object") {
		return fmt.Errorf("%w: mixed character data", errUnexpectedDeleteXMLElement)
	}
	return nil
}

func (s *deleteXMLValidationState) parent() string {
	if len(s.stack) == 0 {
		return ""
	}
	return s.stack[len(s.stack)-1]
}

func allowedDeleteNamespace(namespace string) bool {
	return namespace == "" || namespace == s3XMLNamespace
}

func allowedDeleteAttributes(parent string, element xml.StartElement) bool {
	if len(element.Attr) == 0 {
		return true
	}
	if parent != "" || len(element.Attr) != 1 {
		return false
	}
	attribute := element.Attr[0]
	return attribute.Name.Space == "" &&
		attribute.Name.Local == "xmlns" &&
		attribute.Value == s3XMLNamespace
}

func allowedDeleteElement(parent, element string) bool {
	switch parent {
	case "":
		return element == "Delete"
	case "Delete":
		return element == "Object" || element == "Quiet"
	case "Object":
		return element == "Key" || element == "ETag"
	default:
		return false
	}
}

func (h *S3Handler) deleteObjectItem(
	c *gin.Context,
	bucket Bucket,
	object deleteObjectRequest,
) *s3base.APIError {
	if err := validateHistoricalObjectKeyBoundary(bucket.Name, object.Key); err != nil {
		return objectNameError(err)
	}
	objectPath := "/" + bucket.Name + "/" + object.Key
	if err := validateNewObjectKey(object.Key); err != nil {
		if _, statErr := h.fmgr.StatS3Object(c.Request.Context(), objectPath); statErr != nil {
			return objectNameError(err)
		}
	}
	unlock := h.locks.lock(objectPath)
	defer unlock()
	condition := &filemgr.S3Condition{IfMatch: strings.TrimSpace(object.ETag)}
	if _, err := h.fmgr.DeleteS3Object(c.Request.Context(), objectPath, condition); err != nil {
		return mutationError(err)
	}
	return nil
}
