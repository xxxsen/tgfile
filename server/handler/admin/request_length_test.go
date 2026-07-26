package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPreserveUnknownContentLengthOnlyForStreamingAdminUploads(t *testing.T) {
	for _, test := range []struct {
		method     string
		path       string
		normalized bool
	}{
		{http.MethodPut, "/_admin/api/v1/content", true},
		{http.MethodPost, "/_admin/api/v1/backup/imports", true},
		{http.MethodPost, "/_admin/api/v1/session", false},
		{http.MethodPut, "/hackmd/object", false},
	} {
		t.Run(test.method+" "+test.path, func(t *testing.T) {
			request := httptest.NewRequestWithContext(t.Context(), test.method, test.path, nil)
			request.ContentLength = -1
			prepared := PreserveUnknownContentLength(request)
			if test.normalized {
				require.Equal(t, int64(0), prepared.ContentLength)
			} else {
				require.Equal(t, int64(-1), prepared.ContentLength)
			}
			require.Equal(t, int64(-1), requestContentLength(prepared))
		})
	}
}
