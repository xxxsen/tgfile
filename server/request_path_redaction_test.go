package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

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
	}
	for _, test := range tests {
		request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, test.target, nil)
		originalPath := request.URL.Path
		originalTarget := request.RequestURI

		redacted := service.requestWithRedactedLogPath(request)

		require.Equal(t, test.expected, redacted.URL.Path)
		require.Equal(t, originalTarget, redacted.RequestURI)
		require.Equal(t, originalPath, request.URL.Path)
		stored, exists := redacted.Context().Value(originalRequestPathKey{}).(originalRequestPath)
		require.True(t, exists)
		require.Equal(t, originalPath, stored.path)
	}
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
