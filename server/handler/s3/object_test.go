package s3

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xxxsen/tgfile/entity"
	"github.com/xxxsen/tgfile/filemgr"
)

type objectTestFileManager struct {
	filemgr.IFileManager
	statFileLink func(context.Context, string) (*entity.FileLinkMeta, error)
	openCalls    int
}

func (m *objectTestFileManager) StatFileLink(
	ctx context.Context, link string,
) (*entity.FileLinkMeta, error) {
	return m.statFileLink(ctx, link)
}

func (m *objectTestFileManager) OpenFile(
	context.Context, uint64,
) (io.ReadSeekCloser, error) {
	m.openCalls++
	return nil, errors.New("OpenFile must not be called by HeadObject")
}

func serveObjectRequest(
	t *testing.T, method, target string, manager filemgr.IFileManager,
) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handler := NewS3Handler(manager)
	router.GET("/hackmd/*object", handler.DownloadObject)
	router.HEAD("/hackmd/*object", handler.HeadObject)

	request := httptest.NewRequest(method, target, nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func TestHeadObjectReturnsMetadataWithoutOpeningFile(t *testing.T) {
	mtime := time.Date(2025, time.January, 2, 3, 4, 5, 0, time.UTC)
	manager := &objectTestFileManager{
		statFileLink: func(_ context.Context, link string) (*entity.FileLinkMeta, error) {
			assert.Equal(t, "/hackmd/reports/sample.pdf", link)
			return &entity.FileLinkMeta{
				FileName: "sample.pdf",
				FileId:   42,
				FileSize: 511154,
				Mtime:    mtime.UnixMilli(),
			}, nil
		},
	}

	recorder := serveObjectRequest(
		t, http.MethodHead, "/hackmd/reports/sample.pdf", manager,
	)

	require.Equal(t, http.StatusOK, recorder.Code)
	assert.Empty(t, recorder.Body.String())
	assert.Equal(t, "511154", recorder.Header().Get("Content-Length"))
	assert.Equal(t, "application/pdf", recorder.Header().Get("Content-Type"))
	assert.Equal(t, "bytes", recorder.Header().Get("Accept-Ranges"))
	assert.Equal(t, mtime.Format(http.TimeFormat), recorder.Header().Get("Last-Modified"))
	assert.Equal(t, `W/"42"`, recorder.Header().Get("ETag"))
	assert.Equal(t, "public, max-age=604800", recorder.Header().Get("Cache-Control"))
	assert.Zero(t, manager.openCalls)
}

func TestHeadObjectErrorResponsesHaveNoBody(t *testing.T) {
	tests := []struct {
		name       string
		statResult *entity.FileLinkMeta
		statErr    error
		status     int
	}{
		{
			name:    "missing object",
			statErr: os.ErrNotExist,
			status:  http.StatusNotFound,
		},
		{
			name:    "metadata failure",
			statErr: errors.New("database unavailable"),
			status:  http.StatusInternalServerError,
		},
		{
			name:       "directory is not an object",
			statResult: &entity.FileLinkMeta{FileName: "reports", IsDir: true},
			status:     http.StatusNotFound,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager := &objectTestFileManager{
				statFileLink: func(context.Context, string) (*entity.FileLinkMeta, error) {
					return test.statResult, test.statErr
				},
			}

			recorder := serveObjectRequest(
				t, http.MethodHead, "/hackmd/reports/missing.pdf", manager,
			)

			assert.Equal(t, test.status, recorder.Code)
			assert.Empty(t, recorder.Body.String())
			assert.Zero(t, manager.openCalls)
		})
	}
}

func TestDownloadObjectReturnsS3NotFoundError(t *testing.T) {
	manager := &objectTestFileManager{
		statFileLink: func(context.Context, string) (*entity.FileLinkMeta, error) {
			return nil, os.ErrNotExist
		},
	}

	recorder := serveObjectRequest(
		t, http.MethodGet, "/hackmd/reports/missing.pdf", manager,
	)

	require.Equal(t, http.StatusNotFound, recorder.Code)
	assert.Contains(t, recorder.Body.String(), "<Code>NoSuchKey</Code>")
	assert.Contains(t, recorder.Body.String(), "<Key>reports/missing.pdf</Key>")
	assert.NotContains(t, recorder.Body.String(), "get mapping info fail")
	assert.Zero(t, manager.openCalls)
}
