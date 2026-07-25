package s3

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
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

type uploadTestFileManager struct {
	filemgr.IFileManager
	mutex       sync.Mutex
	mappings    map[string]uint64
	createCalls int
	nextFileID  uint64
}

func newUploadTestFileManager() *uploadTestFileManager {
	return &uploadTestFileManager{
		mappings:   make(map[string]uint64),
		nextFileID: 1,
	}
}

func (m *uploadTestFileManager) StatFileLink(_ context.Context, link string) (*entity.FileLinkMeta, error) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	fileID, ok := m.mappings[link]
	if !ok {
		return nil, os.ErrNotExist
	}
	return &entity.FileLinkMeta{FileId: fileID, FileName: link}, nil
}

func (m *uploadTestFileManager) CreateFile(_ context.Context, size int64, reader io.Reader) (uint64, error) {
	raw, err := io.ReadAll(reader)
	if err != nil {
		return 0, err
	}
	if int64(len(raw)) != size {
		return 0, filemgr.ErrFileShortRead
	}
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.createCalls++
	fileID := m.nextFileID
	m.nextFileID++
	return fileID, nil
}

func (m *uploadTestFileManager) CreateFileLink(_ context.Context, link string, fileID uint64, _ int64, _ bool) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	if _, exists := m.mappings[link]; exists {
		return os.ErrExist
	}
	m.mappings[link] = fileID
	return nil
}

func (m *uploadTestFileManager) counts() (int, int) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	return m.createCalls, len(m.mappings)
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

	request := httptest.NewRequestWithContext(t.Context(), method, target, nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func serveUploadRequest(
	t *testing.T,
	manager filemgr.IFileManager,
	target string,
	content []byte,
) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handler := NewS3Handler(manager)
	router.PUT("/hackmd/*object", handler.UploadObject)
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPut, target, bytes.NewReader(content))
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

func TestUploadObjectRejectsExistingObjectBeforeUpload(t *testing.T) {
	manager := newUploadTestFileManager()
	manager.mappings["/hackmd/existing.txt"] = 42

	recorder := serveUploadRequest(t, manager, "/hackmd/existing.txt", []byte("new content"))

	require.Equal(t, http.StatusConflict, recorder.Code)
	require.Contains(t, recorder.Body.String(), "<Code>OperationAborted</Code>")
	createCalls, mappingCount := manager.counts()
	require.Zero(t, createCalls)
	require.Equal(t, 1, mappingCount)
	require.Equal(t, uint64(42), manager.mappings["/hackmd/existing.txt"])
}

func TestUploadObjectRequiresContentLength(t *testing.T) {
	manager := newUploadTestFileManager()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.PUT("/hackmd/*object", NewS3Handler(manager).UploadObject)
	request := httptest.NewRequestWithContext(
		t.Context(),
		http.MethodPut,
		"/hackmd/file.txt",
		bytes.NewReader([]byte("value")),
	)
	request.ContentLength = -1
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusLengthRequired, recorder.Code)
	require.Contains(t, recorder.Body.String(), "<Code>MissingContentLength</Code>")
	createCalls, mappingCount := manager.counts()
	require.Zero(t, createCalls)
	require.Zero(t, mappingCount)
}

func TestConcurrentUploadSamePathCreatesOneObject(t *testing.T) {
	manager := newUploadTestFileManager()
	handler := NewS3Handler(manager)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.PUT("/hackmd/*object", handler.UploadObject)

	statuses := make(chan int, 2)
	var wait sync.WaitGroup
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			request := httptest.NewRequestWithContext(
				t.Context(),
				http.MethodPut,
				"/hackmd/concurrent.txt",
				bytes.NewReader([]byte("value")),
			)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			statuses <- recorder.Code
		}()
	}
	wait.Wait()
	close(statuses)

	seen := make(map[int]int)
	for status := range statuses {
		seen[status]++
	}
	require.Equal(t, 1, seen[http.StatusOK])
	require.Equal(t, 1, seen[http.StatusConflict])
	createCalls, mappingCount := manager.counts()
	require.Equal(t, 1, createCalls)
	require.Equal(t, 1, mappingCount)
}

func TestPathLockerSerializesSamePathOnly(t *testing.T) {
	locker := newPathLocker()
	unlockA := locker.lock("/a")
	releasedA := false
	t.Cleanup(func() {
		if !releasedA {
			unlockA()
		}
	})

	acquiredSame := make(chan func(), 1)
	go func() {
		acquiredSame <- locker.lock("/a")
	}()
	select {
	case <-acquiredSame:
		t.Fatal("same path lock must block")
	case <-time.After(20 * time.Millisecond):
	}

	acquiredOther := make(chan func(), 1)
	go func() {
		acquiredOther <- locker.lock("/b")
	}()
	select {
	case unlockB := <-acquiredOther:
		unlockB()
	case <-time.After(time.Second):
		t.Fatal("different path lock must not block")
	}

	unlockA()
	releasedA = true
	select {
	case unlockSame := <-acquiredSame:
		unlockSame()
	case <-time.After(time.Second):
		t.Fatal("same path lock did not unblock")
	}
}
