package webdav

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/xxxsen/common/webapi/proxyutil"
	"github.com/xxxsen/tgfile/filemgr"
	"github.com/xxxsen/tgfile/server/model"

	"github.com/gin-gonic/gin"
)

func (h *WebdavHandler) Handler(c *gin.Context) {
	//davRoot: 指定映射到底层存储的路径, 对文件的任何操作均会拼接这个路径
	//webRoot: 指定外部gin处理的路径

	switch c.Request.Method {
	case http.MethodGet:
		h.handleGet(c)
	case http.MethodPut:
		h.handlePut(c)
	case http.MethodDelete:
		h.handleDelete(c)
	case http.MethodHead:
		h.handleHead(c)
	case "PROPFIND":
		h.handlePropfind(c)
	case "PROPPATCH":
		h.handlePropPatch(c)
	case "COPY":
		h.handleCopy(c)
	case "MOVE":
		h.handleMove(c)
	case "MKCOL":
		h.handleMkcol(c)
	case "OPTIONS":
		h.handleOption(c)
	default:
		proxyutil.FailStatus(c, http.StatusForbidden, fmt.Errorf("unsupported method:%s", c.Request.Method))
	}

}

type WebdavHandler struct {
	fmgr    filemgr.IFileManager
	davRoot string
	webRoot string
}

// buildSrcPath 通过url.Path来构建路径
func (h *WebdavHandler) buildSrcPath(c *gin.Context) string {
	p := strings.TrimPrefix(c.Request.URL.Path, h.webRoot)
	return path.Join(h.davRoot, path.Clean(p))
}

// buildDstPath 通过header中的Destination来构建路径
func (h *WebdavHandler) tryBuildDstPath(c *gin.Context) (string, error) {
	link := c.GetHeader("Destination")
	uri, err := url.Parse(link)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(uri.Path, h.webRoot) {
		return "", fmt.Errorf("no webroot in dst path, dst:%s", link)
	}
	p := strings.TrimPrefix(uri.Path, h.webRoot)
	return path.Join(h.davRoot, path.Clean(p)), nil
}

func NewWebdavHandler(fmgr filemgr.IFileManager, davRoot string, webRoot string) *WebdavHandler {
	if len(strings.TrimSpace(davRoot)) == 0 {
		davRoot = "/"
	}

	h := &WebdavHandler{
		fmgr:    fmgr,
		davRoot: davRoot,
		webRoot: webRoot,
	}
	if err := h.initWebdav(davRoot); err != nil {
		panic(err)
	}
	return h
}

func (h *WebdavHandler) initWebdav(root string) error {
	if err := h.fmgr.CreateFileLink(context.Background(), root, 0, 0, true); err != nil {
		return err
	}
	return nil
}

func (h *WebdavHandler) writeDavResponse(c *gin.Context, res *model.Multistatus) error {
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
