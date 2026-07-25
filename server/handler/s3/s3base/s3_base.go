package s3base

import (
	"encoding/xml"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/xxxsen/common/logutil"
	"github.com/xxxsen/common/trace"
	"go.uber.org/zap"
)

var errNilAPIError = errors.New("nil S3 API error")

type APIError struct {
	HTTPStatus          int
	Code                string
	Message             string
	Bucket              string
	Key                 string
	Resource            string
	PartNumberRequested int
	ActualPartCount     int
	Cause               error
}

func (e *APIError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return e.Code + ": " + e.Message
}

func (e *APIError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

type ErrorResponse struct {
	XMLName             xml.Name `xml:"Error"`
	Code                string   `xml:"Code"`
	Message             string   `xml:"Message"`
	BucketName          string   `xml:"BucketName,omitempty"`
	Key                 string   `xml:"Key,omitempty"`
	Resource            string   `xml:"Resource,omitempty"`
	PartNumberRequested int      `xml:"PartNumberRequested,omitempty"`
	ActualPartCount     int      `xml:"ActualPartCount,omitempty"`
	RequestID           string   `xml:"RequestId"`
	HostID              string   `xml:"HostId"`
}

func NewError(status int, code, message string, cause error) *APIError {
	return &APIError{
		HTTPStatus: status,
		Code:       code,
		Message:    message,
		Cause:      cause,
	}
}

func WriteError(c *gin.Context, apiError *APIError) {
	if apiError == nil {
		apiError = InternalError(errNilAPIError)
	}
	requestID, _ := trace.GetTraceId(c.Request.Context())
	logutil.GetLogger(c.Request.Context()).Error(
		"S3 request failed",
		zap.Bool("has_cause", apiError.Cause != nil),
		zap.String("code", apiError.Code),
		zap.Int("status_code", apiError.HTTPStatus),
	)
	c.Set("s3-result-code", apiError.Code)
	c.Header("x-amz-request-id", requestID)
	if c.Request.Method == http.MethodHead || apiError.HTTPStatus == http.StatusNotModified {
		c.Status(apiError.HTTPStatus)
		return
	}
	c.XML(apiError.HTTPStatus, &ErrorResponse{
		Code:                apiError.Code,
		Message:             apiError.Message,
		BucketName:          apiError.Bucket,
		Key:                 apiError.Key,
		Resource:            apiError.Resource,
		PartNumberRequested: apiError.PartNumberRequested,
		ActualPartCount:     apiError.ActualPartCount,
		RequestID:           requestID,
		HostID:              requestID,
	})
}

func InternalError(cause error) *APIError {
	return NewError(
		http.StatusInternalServerError,
		"InternalError",
		"We encountered an internal error. Please try again.",
		cause,
	)
}

func NoSuchKey(cause error) *APIError {
	return NewError(http.StatusNotFound, "NoSuchKey", "The specified key does not exist.", cause)
}

func AccessDenied(cause error) *APIError {
	return NewError(http.StatusForbidden, "AccessDenied", "Access Denied.", cause)
}

func InvalidRequest(message string, cause error) *APIError {
	return NewError(http.StatusBadRequest, "InvalidRequest", message, cause)
}

func PreconditionFailed(cause error) *APIError {
	return NewError(
		http.StatusPreconditionFailed,
		"PreconditionFailed",
		"At least one of the preconditions you specified did not hold.",
		cause,
	)
}

func InvalidRange(cause error) *APIError {
	return NewError(
		http.StatusRequestedRangeNotSatisfiable,
		"InvalidRange",
		"The requested range is not satisfiable.",
		cause,
	)
}

func SimpleReply(ctx *gin.Context) {
	data := []byte("<?xml version=\"1.0\" encoding=\"UTF-8\"?>" +
		"<LocationConstraint xmlns=\"http://s3.amazonaws.com/doc/2006-03-01/\"></LocationConstraint>")
	ctx.Data(http.StatusOK, "application/xml", data)
}
