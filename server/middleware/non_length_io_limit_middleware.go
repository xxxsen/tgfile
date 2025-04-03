package middleware

import (
	"bytes"
	"fmt"
	"io"
	"net/http"

	"github.com/xxxsen/tgfile/proxyutil"

	"github.com/gin-gonic/gin"
	"github.com/xxxsen/common/logutil"
	"go.uber.org/zap"
)

const (
	defaultMaxAllowChunkStreamLength = 5 * 1024 * 1024 //5MB
)

func NonLengthIOLimitMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.ContentLength >= 0 {
			c.Next()
			return
		}
		ctx := c.Request.Context()
		logutil.GetLogger(ctx).Debug("recv non-content-length io request")
		if len(c.Request.TransferEncoding) == 0 || c.Request.TransferEncoding[0] != "chunked" {
			proxyutil.FailStatus(c, http.StatusBadRequest, fmt.Errorf("only chunked encoding can use content-length = -1"))
			return
		}
		data, err := io.ReadAll(io.LimitReader(c.Request.Body, defaultMaxAllowChunkStreamLength+1))
		if err != nil {
			proxyutil.FailStatus(c, http.StatusBadRequest, fmt.Errorf("read client data failed, err:%w", err))
			return
		}
		logutil.GetLogger(ctx).Debug("read chunk stream from client", zap.Int("length", len(data)))
		if len(data) > defaultMaxAllowChunkStreamLength {
			proxyutil.FailStatus(c, http.StatusBadRequest, fmt.Errorf("chunk stream exceed length limit"))
			return
		}
		r := bytes.NewReader(data)
		clr := c.Request.Body
		rc := &readCloserWrap{
			r: r,
			c: clr,
		}
		c.Request.Body = rc
		c.Request.ContentLength = int64(len(data))
	}
}

type readCloserWrap struct {
	r io.Reader
	c io.Closer
}

func (c *readCloserWrap) Read(p []byte) (n int, err error) {
	return c.r.Read(p)
}

func (c *readCloserWrap) Close() error {
	return c.c.Close()
}
