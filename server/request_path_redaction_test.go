package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/xxxsen/tgfile/server/handler/s3"
)

func TestSensitiveRequestPathsAreRedactedWithoutChangingRequestTarget(t *testing.T) {
	service := &Server{s3: s3.NewS3Handler(nil, s3.Config{
		Buckets: []s3.Bucket{{Name: "hackmd", ACL: s3.BucketACLPublicRead}},
	})}
	tests := []struct {
		target   string
		expected string
	}{
		{
			target:   "http://example.test/hackmd/private/path.bin?partNumber=1",
			expected: "/hackmd/_redacted_",
		},
		{
			target:   "http://example.test/file/download/0123456789abcdef-secret.txt",
			expected: "/file/download/_redacted_",
		},
		{
			target:   "http://example.test/file/meta/0123456789abcdef-secret.txt",
			expected: "/file/meta/_redacted_",
		},
		{
			target:   "http://example.test/webdav/hackmd/private/path.bin",
			expected: "/webdav/_redacted_",
		},
		{
			target:   "http://example.test/static/defaults/01/private.bin",
			expected: "/static/_redacted_",
		},
		{
			target:   "http://example.test/unknown/private.bin",
			expected: "/unknown/_redacted_",
		},
		{
			target:   "http://example.test/_admin/api/v1/content?path=%2Fsecret.bin",
			expected: "/_admin/api/_redacted_",
		},
	}
	for _, test := range tests {
		request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, test.target, nil)
		originalPath := request.URL.Path
		originalTarget := request.RequestURI

		redacted := service.requestWithRedactedLogPath(request)

		require.Equal(t, test.expected, redacted.URL.Path)
		require.Equal(t, originalTarget, redacted.RequestURI)
		require.Equal(t, originalPath, request.URL.Path)
		if test.expected == "/_admin/api/_redacted_" {
			require.Empty(t, redacted.URL.RawQuery)
			require.Equal(t, "path=%2Fsecret.bin", request.URL.RawQuery)
		}
		stored, exists := redacted.Context().Value(originalRequestPathKey{}).(originalRequestPath)
		require.True(t, exists)
		require.Equal(t, originalPath, stored.path)
	}
}

func TestOriginalPathIsVisibleOnlyInsideRestoreMiddleware(t *testing.T) {
	service := &Server{}
	request := httptest.NewRequestWithContext(
		t.Context(),
		http.MethodGet,
		"http://example.test/_admin/api/v1/content?path=%2Fsecret.bin",
		nil,
	)
	request = service.requestWithRedactedLogPath(request)
	response := httptest.NewRecorder()
	engine := gin.New()
	var (
		innerPath, innerQuery string
		outerPath, outerQuery string
	)
	engine.Use(
		func(context *gin.Context) {
			context.Next()
			outerPath = context.Request.URL.Path
			outerQuery = context.Request.URL.RawQuery
		},
		restoreOriginalRequestPath,
	)
	engine.Any("/_admin/api/*all", func(context *gin.Context) {
		innerPath = context.Request.URL.Path
		innerQuery = context.Request.URL.RawQuery
		context.Status(http.StatusNoContent)
	})
	engine.ServeHTTP(response, request)

	require.Equal(t, http.StatusNoContent, response.Code)
	require.Equal(t, "/_admin/api/v1/content", innerPath)
	require.Equal(t, "path=%2Fsecret.bin", innerQuery)
	require.Equal(t, "/_admin/api/_redacted_", outerPath)
	require.Empty(t, outerQuery)
}

func TestNonSensitiveRequestPathsAreNotCloned(t *testing.T) {
	service := &Server{s3: s3.NewS3Handler(nil, s3.Config{
		Buckets: []s3.Bucket{{Name: "hackmd", ACL: s3.BucketACLPublicRead}},
	})}
	for _, target := range []string{
		"http://example.test/",
		"http://example.test/hackmd",
		"http://example.test/hackmd/",
		"http://example.test/file/upload",
	} {
		request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
		require.Same(t, request, service.requestWithRedactedLogPath(request))
	}
}
