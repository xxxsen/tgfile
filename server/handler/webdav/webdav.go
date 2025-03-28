package webdav

import (
	"context"
	"encoding/xml"
	"net/http"
	"sync"
	"tgfile/filemgr"
	"tgfile/server/model"

	"github.com/gin-gonic/gin"
	"github.com/xxxsen/common/logutil"
	"go.uber.org/zap"
)

var (
	initOnce sync.Once
)

func initWebdav() {
	initOnce.Do(func() {
		if err := filemgr.CreateLink(context.Background(), "/webdav", 0, 0, true); err != nil {
			panic(err)
		}
	})
}

func Handler(c *gin.Context) {
	initWebdav()
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

func writeDavResponse(c *gin.Context, res *model.Multistatus) error {
	c.Status(http.StatusMultiStatus)
	if _, err := c.Writer.Write([]byte(xml.Header)); err != nil {
		return err
	}
	raw, _ := xml.Marshal(res)
	if _, err := c.Writer.Write(raw); err != nil {
		return err
	}
	return nil
}
