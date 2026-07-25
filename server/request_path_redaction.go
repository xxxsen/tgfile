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
	path    string
	rawPath string
}

func (s *Server) requestWithRedactedLogPath(request *http.Request) *http.Request {
	redacted, sensitive := s.redactedRequestPath(request.URL.Path)
	if !sensitive {
		return request
	}
	original := originalRequestPath{
		path:    request.URL.Path,
		rawPath: request.URL.RawPath,
	}
	ctx := context.WithValue(request.Context(), originalRequestPathKey{}, original)
	clone := request.Clone(ctx)
	cloneURL := *request.URL
	cloneURL.Path = redacted
	cloneURL.RawPath = ""
	clone.URL = &cloneURL
	return clone
}

func (s *Server) redactedRequestPath(requestPath string) (string, bool) {
	for _, prefix := range []string{"/file/download/", "/file/meta/"} {
		if strings.HasPrefix(requestPath, prefix) && len(requestPath) > len(prefix) {
			return prefix + redactedPathComponent, true
		}
	}
	for _, prefix := range []string{"/webdav/", "/static/"} {
		if strings.HasPrefix(requestPath, prefix) && len(requestPath) > len(prefix) {
			return prefix + redactedPathComponent, true
		}
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
	case "", "backup", "file", "static", "webdav":
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
	cloneURL := *c.Request.URL
	cloneURL.Path = original.path
	cloneURL.RawPath = original.rawPath
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
	case strings.HasPrefix(requestPath, "/static/"):
		setPathParameter(c, "filepath", strings.TrimPrefix(requestPath, "/static"))
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
