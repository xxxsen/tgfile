package s3base

import (
	"encoding/xml"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/xxxsen/common/logutil"
	"github.com/xxxsen/common/trace"
	"go.uber.org/zap"
)

type S3ErrorMessage struct {
	XMLName    xml.Name `xml:"Error"`
	Code       string   `xml:"Code"`
	Message    string   `xml:"Message"`
	Key        string   `xml:"Key"`
	BucketName string   `xml:"BucketName"`
	Resource   string   `xml:"Resource"`
	RequestId  string   `xml:"RequestId"`
	HostId     string   `xml:"HostId"`
}

func ResponseWithError(ctx *gin.Context, code int, e *S3ErrorMessage) {
	ctx.XML(code, e)
}

func SimpleReply(ctx *gin.Context) {
	data := []byte("<?xml version=\"1.0\" encoding=\"UTF-8\"?>" +
		"<LocationConstraint xmlns=\"http://s3.amazonaws.com/doc/2006-03-01/\"></LocationConstraint>")
	_, err := ctx.Writer.Write(data)
	if err != nil {
		logutil.GetLogger(ctx).Error("write msg fail", zap.Error(err))
		return
	}
}

func logError(c *gin.Context, statuscode int, err error) string {
	ctx := c.Request.Context()
	logutil.GetLogger(ctx).Error("write err to client",
		zap.Error(err),
		zap.Int("status_code", statuscode))
	traceid, _ := trace.GetTraceId(ctx)
	return traceid
}

func WriteError(c *gin.Context, statuscode int, err error) {
	traceid := logError(c, statuscode, err)
	code := ErrInternalService
	message := "We encountered an internal error. Please try again."
	switch statuscode {
	case http.StatusNotFound:
		code = ErrFileNotFound
		message = "The specified key does not exist."
	case http.StatusConflict:
		code = ErrOperationAborted
		message = "A conflicting operation is already in progress or the object already exists."
	case http.StatusLengthRequired:
		code = ErrMissingContentSize
		message = "You must provide the Content-Length HTTP header."
	case http.StatusBadRequest:
		code = ErrInvalidRequest
		message = "The request is invalid."
	}
	e := &S3ErrorMessage{
		Code:      code,
		Message:   message,
		Key:       strings.TrimPrefix(c.Param("object"), "/"),
		Resource:  c.Request.URL.Path,
		RequestId: traceid,
		HostId:    traceid,
	}
	ResponseWithError(c, statuscode, e)
}

func WriteHeadError(c *gin.Context, statuscode int, err error) {
	logError(c, statuscode, err)
	c.Status(statuscode)
}
