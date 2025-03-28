package proxyutil

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

const (
	defaultBizErrCode uint32 = 100000
)

var (
	defaultReplyErrKey = "x-filemgr-reply-err"
)

type CommonResponse struct {
	Code    uint32      `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data"`
}

type iCodeErr interface {
	Code() uint32
}

func makePacket(code uint32, msg string, obj interface{}) *CommonResponse {
	return &CommonResponse{
		Code:    code,
		Message: msg,
		Data:    obj,
	}
}

func SuccessJson(c *gin.Context, obj interface{}) {
	c.JSON(http.StatusOK, makePacket(0, "", obj))
}

func FailJson(c *gin.Context, code int, err error) {
	bizCode := defaultBizErrCode
	errmsg := err.Error()
	if ie, ok := err.(iCodeErr); ok {
		bizCode = ie.Code()
	}
	markReplyErr(c, err)
	c.AbortWithStatusJSON(code, makePacket(bizCode, errmsg, nil))
}

func FailStatus(c *gin.Context, code int, err error) {
	markReplyErr(c, err)
	c.AbortWithStatus(code)
}

func markReplyErr(c *gin.Context, err error) {
	c.Set(defaultReplyErrKey, err)
}

func GetReplyErrInfo(c *gin.Context) error {
	v, ok := c.Get(defaultReplyErrKey)
	if !ok {
		return nil
	}
	return v.(error)
}
