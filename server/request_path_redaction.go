package server

import (
	"context"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

const redactedPathComponent = "_redacted_"

type originalRequestPathKey struct{}

type originalRequestPath struct {
	path     string
	rawPath  string
	rawQuery string
}

func (s *Server) requestWithRedactedLogPath(request *http.Request) *http.Request {
	redacted, sensitive := s.redactedRequestPath(request.URL.Path)
	if !sensitive {
		return request
	}
	original := originalRequestPath{
		path:     request.URL.Path,
		rawPath:  request.URL.RawPath,
		rawQuery: request.URL.RawQuery,
	}
	ctx := context.WithValue(request.Context(), originalRequestPathKey{}, original)
	clone := request.Clone(ctx)
	cloneURL := *request.URL
	cloneURL.Path = redacted
	cloneURL.RawPath = ""
	if strings.HasPrefix(request.URL.Path, "/_admin/api/") {
		cloneURL.RawQuery = ""
	}
	clone.URL = &cloneURL
	return clone
}

func (s *Server) redactedRequestPath(requestPath string) (string, bool) {
	if strings.HasPrefix(requestPath, "/_admin/api/") {
		return "/_admin/api/" + redactedPathComponent, true
	}
	for _, prefix := range []string{"/file/download/", "/file/meta/"} {
		if strings.HasPrefix(requestPath, prefix) && len(requestPath) > len(prefix) {
			return prefix + redactedPathComponent, true
		}
	}
	const webDAVPrefix = "/webdav/"
	if strings.HasPrefix(requestPath, webDAVPrefix) && len(requestPath) > len(webDAVPrefix) {
		return webDAVPrefix + redactedPathComponent, true
	}
	if s.s3 == nil {
		return requestPath, false
	}
	trimmed := strings.TrimPrefix(requestPath, "/")
	bucketName, object, hasObject := strings.Cut(trimmed, "/")
	if !hasObject || object == "" {
		return requestPath, false
	}
	switch bucketName {
	case "", "_admin", "backup", "file", "webdav":
		return requestPath, false
	}
	return "/" + bucketName + "/" + redactedPathComponent, true
}

func restoreOriginalRequestPath(c *gin.Context) {
	original, exists := c.Request.Context().Value(originalRequestPathKey{}).(originalRequestPath)
	if !exists {
		c.Next()
		return
	}
	redacted := originalRequestPath{
		path:     c.Request.URL.Path,
		rawPath:  c.Request.URL.RawPath,
		rawQuery: c.Request.URL.RawQuery,
	}
	redactedParams := append(gin.Params(nil), c.Params...)
	defer func() {
		c.Request.URL.Path = redacted.path
		c.Request.URL.RawPath = redacted.rawPath
		c.Request.URL.RawQuery = redacted.rawQuery
		c.Params = redactedParams
	}()
	cloneURL := *c.Request.URL
	cloneURL.Path = original.path
	cloneURL.RawPath = original.rawPath
	cloneURL.RawQuery = original.rawQuery
	c.Request.URL = &cloneURL
	restoreOriginalPathParameters(c, original.path)
	c.Next()
}

func restoreOriginalPathParameters(c *gin.Context, requestPath string) {
	switch {
	case strings.HasPrefix(requestPath, "/file/download/"):
		setPathParameter(c, "key", strings.TrimPrefix(requestPath, "/file/download/"))
	case strings.HasPrefix(requestPath, "/file/meta/"):
		setPathParameter(c, "key", strings.TrimPrefix(requestPath, "/file/meta/"))
	case strings.HasPrefix(requestPath, "/webdav/"):
		setPathParameter(c, "all", strings.TrimPrefix(requestPath, "/webdav"))
	default:
		trimmed := strings.TrimPrefix(requestPath, "/")
		bucketName, _, hasObject := strings.Cut(trimmed, "/")
		if hasObject {
			setPathParameter(c, "object", strings.TrimPrefix(requestPath, "/"+bucketName))
		}
	}
}

func setPathParameter(c *gin.Context, name, value string) {
	for index := range c.Params {
		if c.Params[index].Key == name {
			c.Params[index].Value = value
			return
		}
	}
}
