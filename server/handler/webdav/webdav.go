package webdav

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/xxxsen/common/logutil"
	"go.uber.org/zap"
)

func Handler(c *gin.Context) {
	switch c.Request.Method {
	case http.MethodGet:
		handleGet(c)
	case http.MethodPut:
		handlePut(c)
	case http.MethodDelete:
		handleDelete(c)
	case http.MethodHead:
		handleHead(c)
	case "PROPFIND":
		handlePropfind(c)
	case "PROPPATCH":
		handlePropPatch(c)
	case "COPY":
		handleCopy(c)
	case "MOVE":
		handleMove(c)
	case "MKCOL":
		handleMkcol(c)
	default:
		c.AbortWithStatus(http.StatusForbidden)
		logutil.GetLogger(c.Request.Context()).Error("unsupported method", zap.String("method", c.Request.Method))
	}
}
