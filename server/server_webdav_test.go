package server

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xxxsen/tgfile/authz"
)

func TestNewWithDefaultConfigDoesNotPanic(t *testing.T) {
	instance, err := New("127.0.0.1:0")
	require.NoError(t, err)
	require.NotNil(t, instance)

	response := httptest.NewRecorder()
	instance.ServeHTTP(response, httptest.NewRequestWithContext(
		t.Context(),
		http.MethodGet,
		"/missing",
		nil,
	))
	assert.Equal(t, http.StatusNotFound, response.Code)
}

func TestIsWebDAVRequestPathUsesSegmentBoundary(t *testing.T) {
	assert.True(t, isWebDAVRequestPath("/webdav"))
	assert.True(t, isWebDAVRequestPath("/webdav/"))
	assert.True(t, isWebDAVRequestPath("/webdav/file.txt"))
	assert.False(t, isWebDAVRequestPath("/webdav2"))
	assert.False(t, isWebDAVRequestPath("/webdav-backup/file.txt"))
}

func TestPrepareWebDAVPutRejectsBeforeReadingOrSpooling(t *testing.T) {
	tests := []struct {
		name           string
		authorizations []string
		roles          map[string]string
		wantStatus     int
	}{
		{
			name:       "missing authorization",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "duplicate authorization",
			authorizations: []string{
				basicAuthorization("editor", "secret"),
				basicAuthorization("editor", "secret"),
			},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:           "invalid credentials",
			authorizations: []string{basicAuthorization("editor", "wrong")},
			wantStatus:     http.StatusUnauthorized,
		},
		{
			name:           "read only principal",
			authorizations: []string{basicAuthorization("reader", "read-secret")},
			roles:          map[string]string{"reader": "read"},
			wantStatus:     http.StatusForbidden,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tempDir := t.TempDir()
			body := &countingReadCloser{
				reader: strings.NewReader("must not be read"),
			}
			request := httptest.NewRequestWithContext(
				t.Context(),
				http.MethodPut,
				"http://example.test/webdav/file.bin",
				body,
			)
			request.ContentLength = -1
			request.Header["Authorization"] = test.authorizations
			instance := webDAVPreparationServer(tempDir, test.roles)
			response := httptest.NewRecorder()

			_, closePrepared, ok := instance.prepareWebDAVPut(response, request)

			assert.False(t, ok)
			assert.False(t, closePrepared)
			assert.Equal(t, test.wantStatus, response.Code)
			assert.Zero(t, body.reads)
			require.Empty(t, directoryNames(t, tempDir))
		})
	}
}

func TestPrepareWebDAVPutSpoolsAndCleansAuthorizedUnknownLengthBody(t *testing.T) {
	tempDir := t.TempDir()
	body := &countingReadCloser{reader: strings.NewReader("payload")}
	request := httptest.NewRequestWithContext(
		t.Context(),
		http.MethodPut,
		"http://example.test/webdav/file.bin",
		body,
	)
	request.ContentLength = -1
	request.Header.Set("Authorization", basicAuthorization("editor", "secret"))
	instance := webDAVPreparationServer(
		tempDir,
		map[string]string{"editor": "read-write"},
	)
	response := httptest.NewRecorder()

	prepared, closePrepared, ok := instance.prepareWebDAVPut(response, request)

	require.True(t, ok)
	require.True(t, closePrepared)
	assert.EqualValues(t, len("payload"), prepared.ContentLength)
	assert.Positive(t, body.reads)
	require.Len(t, directoryNames(t, tempDir), 1)
	require.NoError(t, prepared.Body.Close())
	require.Empty(t, directoryNames(t, tempDir))
}

func TestPrepareWebDAVPutRemovesOversizedUnknownLengthSpool(t *testing.T) {
	tempDir := t.TempDir()
	request := httptest.NewRequestWithContext(
		t.Context(),
		http.MethodPut,
		"http://example.test/webdav/file.bin",
		io.NopCloser(strings.NewReader("12345")),
	)
	request.ContentLength = -1
	request.Header.Set("Authorization", basicAuthorization("editor", "secret"))
	instance := webDAVPreparationServer(
		tempDir,
		map[string]string{"editor": "read-write"},
	)
	instance.c.webdav.MaxUploadSize = 4
	response := httptest.NewRecorder()

	_, closePrepared, ok := instance.prepareWebDAVPut(response, request)

	assert.False(t, ok)
	assert.False(t, closePrepared)
	assert.Equal(t, http.StatusRequestEntityTooLarge, response.Code)
	require.Empty(t, directoryNames(t, tempDir))
}

func TestCleanupStaleWebDAVUploadsUsesExactManagedPrefix(t *testing.T) {
	tempDir := t.TempDir()
	staleManaged := filepath.Join(tempDir, "tgfile-webdav-stale")
	staleUnmanaged := filepath.Join(tempDir, "webdav-stale")
	freshManaged := filepath.Join(tempDir, "tgfile-webdav-fresh")
	managedDirectory := filepath.Join(tempDir, "tgfile-webdav-directory")
	for _, path := range []string{staleManaged, staleUnmanaged, freshManaged} {
		require.NoError(t, os.WriteFile(path, []byte("data"), 0o600))
	}
	require.NoError(t, os.Mkdir(managedDirectory, 0o700))
	staleTime := time.Now().Add(-25 * time.Hour)
	require.NoError(t, os.Chtimes(staleManaged, staleTime, staleTime))
	require.NoError(t, os.Chtimes(staleUnmanaged, staleTime, staleTime))
	require.NoError(t, os.Chtimes(managedDirectory, staleTime, staleTime))

	require.NoError(t, cleanupStaleWebDAVUploads(tempDir, 24*time.Hour))

	require.NoFileExists(t, staleManaged)
	require.FileExists(t, staleUnmanaged)
	require.FileExists(t, freshManaged)
	require.DirExists(t, managedDirectory)
}

type countingReadCloser struct {
	reader io.Reader
	reads  int
}

func (r *countingReadCloser) Read(buffer []byte) (int, error) {
	r.reads++
	return r.reader.Read(buffer)
}

func (r *countingReadCloser) Close() error {
	return nil
}

func webDAVPreparationServer(tempDir string, roles map[string]string) *Server {
	permissions := make(map[string][]string, len(roles))
	for username, role := range roles {
		permission := authz.WebDAVRead
		if role == "read-write" {
			permission = authz.WebDAVWrite
		}
		permissions[username] = []string{string(permission)}
	}
	authorizer, err := authz.New(permissions)
	if err != nil {
		panic(err)
	}
	return &Server{c: &config{
		userMap: map[string]string{
			"editor": "secret",
			"reader": "read-secret",
		},
		webdav: WebDAVOptions{
			Enabled:       true,
			UploadTempDir: tempDir,
		},
		authorizer: authorizer,
	}}
}

func basicAuthorization(username, password string) string {
	credentials := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
	return "Basic " + credentials
}

func directoryNames(t *testing.T, directory string) []string {
	t.Helper()
	entries, err := os.ReadDir(directory)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}
