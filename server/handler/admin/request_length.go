package admin

import (
	"context"
	"net/http"
)

type unknownContentLengthKey struct{}

// PreserveUnknownContentLength prevents the common HTTP middleware from
// buffering an admin upload whose length was not declared. The handler still
// sees the original state and returns 411 after authentication and CSRF checks.
func PreserveUnknownContentLength(request *http.Request) *http.Request {
	if request == nil || request.ContentLength >= 0 ||
		!requiresKnownContentLength(request.Method, request.URL.Path) {
		return request
	}
	ctx := context.WithValue(request.Context(), unknownContentLengthKey{}, true)
	cloned := request.Clone(ctx)
	cloned.ContentLength = 0
	return cloned
}

func requiresKnownContentLength(method, requestPath string) bool {
	return method == http.MethodPut && requestPath == "/_admin/api/v1/content" ||
		method == http.MethodPost && requestPath == "/_admin/api/v1/backup/imports"
}

func requestContentLength(request *http.Request) int64 {
	if request != nil {
		if unknown, _ := request.Context().Value(unknownContentLengthKey{}).(bool); unknown {
			return -1
		}
		return request.ContentLength
	}
	return -1
}
