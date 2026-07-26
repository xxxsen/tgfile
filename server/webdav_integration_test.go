package server_test

import (
	"bytes"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xxxsen/tgfile/blockio/mem"
	"github.com/xxxsen/tgfile/db"
	"github.com/xxxsen/tgfile/filemgr"
	"github.com/xxxsen/tgfile/server"
)

func newWebDAVIntegrationEnvironment(
	t *testing.T,
	users map[string]string,
	options server.WebDAVOptions,
	blockSize int64,
) *integrationEnvironment {
	t.Helper()
	databaseClient, err := db.Open(filepath.Join(t.TempDir(), "webdav.db"))
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, databaseClient.Close())
	})
	block, err := mem.New(blockSize)
	require.NoError(t, err)
	cache, err := filemgr.NewFileIOCache(&filemgr.FileIOCacheConfig{
		DisableL1Cache: true,
		DisableL2Cache: true,
	})
	require.NoError(t, err)
	manager := filemgr.NewFileManager(databaseClient, block, cache)
	options.Enabled = true
	if options.Root == "" {
		options.Root = "/"
	}
	handler, err := server.New(
		"127.0.0.1:0",
		server.WithUser(users),
		server.WithS3(server.S3Options{
			Enabled: true,
			Buckets: []server.S3BucketOptions{{
				Name: "hackmd",
				ACL:  server.BucketACLPrivate,
			}},
		}),
		server.WithWebDAV(options),
		server.WithFileManager(manager),
	)
	require.NoError(t, err)
	testServer := httptest.NewServer(handler)
	t.Cleanup(testServer.Close)
	return &integrationEnvironment{
		server:   testServer,
		database: databaseClient,
		manager:  manager,
	}
}

func doWebDAVRequest(
	t *testing.T,
	client *http.Client,
	username, password, method, target string,
	body io.Reader,
	headers map[string]string,
) *webDAVTestResponse {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), method, target, body)
	require.NoError(t, err)
	if username != "" {
		request.SetBasicAuth(username, password)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := client.Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	return &webDAVTestResponse{
		StatusCode: response.StatusCode,
		Header:     response.Header,
		Body:       raw,
	}
}

type webDAVTestResponse struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

func requireWebDAVStatus(t *testing.T, response *webDAVTestResponse, status int) []byte {
	t.Helper()
	require.Equal(t, status, response.StatusCode)
	return response.Body
}

func TestWebDAVClassOnePropertiesAndCrossProtocolLifecycle(t *testing.T) {
	environment := newWebDAVIntegrationEnvironment(
		t,
		map[string]string{"editor": "secret"},
		server.WebDAVOptions{},
		8*1024*1024,
	)
	client := environment.server.Client()
	root := environment.server.URL + "/webdav/dav"
	fileURL := root + "/hello%20世界.txt"

	response := doWebDAVRequest(
		t,
		client,
		"",
		"",
		http.MethodGet,
		fileURL,
		nil,
		nil,
	)
	requireWebDAVStatus(t, response, http.StatusUnauthorized)
	require.Contains(t, response.Header.Get("WWW-Authenticate"), "Basic")
	require.Equal(t, "private, no-cache", response.Header.Get("Cache-Control"))
	require.Equal(t, "Authorization", response.Header.Get("Vary"))

	response = doWebDAVRequest(
		t,
		client,
		"editor",
		"secret",
		http.MethodOptions,
		environment.server.URL+"/webdav/",
		nil,
		nil,
	)
	requireWebDAVStatus(t, response, http.StatusOK)
	require.Equal(t, "1, 2, sync-collection", response.Header.Get("DAV"))
	require.Contains(t, response.Header.Get("Allow"), "PROPPATCH")
	require.Contains(t, response.Header.Get("Allow"), "LOCK")

	response = doWebDAVRequest(
		t,
		client,
		"editor",
		"secret",
		"MKCOL",
		root+"/missing/child",
		nil,
		nil,
	)
	requireWebDAVStatus(t, response, http.StatusConflict)
	response = doWebDAVRequest(
		t,
		client,
		"editor",
		"secret",
		"MKCOL",
		root,
		nil,
		nil,
	)
	requireWebDAVStatus(t, response, http.StatusCreated)
	response = doWebDAVRequest(
		t,
		client,
		"editor",
		"secret",
		"MKCOL",
		root,
		nil,
		nil,
	)
	requireWebDAVStatus(t, response, http.StatusMethodNotAllowed)

	response = doWebDAVRequest(
		t,
		client,
		"editor",
		"secret",
		http.MethodPut,
		fileURL,
		strings.NewReader("first"),
		nil,
	)
	requireWebDAVStatus(t, response, http.StatusCreated)
	etag := response.Header.Get("ETag")
	require.Regexp(t, `^"[0-9]+-5"$`, etag)

	response = doWebDAVRequest(
		t,
		client,
		"editor",
		"secret",
		http.MethodGet,
		fileURL,
		nil,
		map[string]string{"If-None-Match": etag},
	)
	requireWebDAVStatus(t, response, http.StatusNotModified)
	require.Equal(t, "private, no-cache", response.Header.Get("Cache-Control"))
	require.Contains(t, response.Header.Values("Vary"), "Authorization")

	response = doWebDAVRequest(
		t,
		client,
		"editor",
		"secret",
		http.MethodHead,
		fileURL,
		nil,
		nil,
	)
	requireWebDAVStatus(t, response, http.StatusOK)
	require.Equal(t, etag, response.Header.Get("ETag"))
	require.Equal(t, "5", response.Header.Get("Content-Length"))
	require.Equal(t, "bytes", response.Header.Get("Accept-Ranges"))

	propertyUpdate := `<D:propertyupdate xmlns:D="DAV:" xmlns:X="urn:tgfile:test">` +
		`<D:set><D:prop><X:color><X:value>blue</X:value></X:color></D:prop></D:set>` +
		`</D:propertyupdate>`
	response = doWebDAVRequest(
		t,
		client,
		"editor",
		"secret",
		"PROPPATCH",
		fileURL,
		strings.NewReader(propertyUpdate),
		map[string]string{"Content-Type": "application/xml"},
	)
	require.Contains(t, string(requireWebDAVStatus(t, response, http.StatusMultiStatus)), "200 OK")

	propfind := `<D:propfind xmlns:D="DAV:" xmlns:X="urn:tgfile:test"><D:prop>` +
		`<D:getetag/><D:getlastmodified/><X:color/><X:missing/>` +
		`</D:prop></D:propfind>`
	response = doWebDAVRequest(
		t,
		client,
		"editor",
		"secret",
		"PROPFIND",
		fileURL,
		strings.NewReader(propfind),
		map[string]string{"Depth": "0", "Content-Type": "application/xml"},
	)
	propfindBody := requireWebDAVStatus(t, response, http.StatusMultiStatus)
	require.NoError(t, xml.Unmarshal(propfindBody, &struct{}{}))
	require.Contains(t, string(propfindBody), strings.Trim(etag, `"`))
	require.Contains(t, string(propfindBody), "blue")
	require.Contains(t, string(propfindBody), "404 Not Found")

	response = doWebDAVRequest(
		t,
		client,
		"editor",
		"secret",
		http.MethodPut,
		fileURL,
		strings.NewReader("second"),
		map[string]string{"If-Match": etag},
	)
	requireWebDAVStatus(t, response, http.StatusNoContent)
	response = doWebDAVRequest(
		t,
		client,
		"editor",
		"secret",
		"PROPFIND",
		fileURL,
		strings.NewReader(propfind),
		map[string]string{"Depth": "0"},
	)
	require.Contains(
		t,
		string(requireWebDAVStatus(t, response, http.StatusMultiStatus)),
		"blue",
	)

	copyURL := root + "/copy.txt"
	response = doWebDAVRequest(
		t,
		client,
		"editor",
		"secret",
		"COPY",
		fileURL,
		nil,
		map[string]string{"Destination": copyURL, "Overwrite": "F"},
	)
	requireWebDAVStatus(t, response, http.StatusCreated)
	response = doWebDAVRequest(
		t,
		client,
		"editor",
		"secret",
		"PROPFIND",
		copyURL,
		strings.NewReader(propfind),
		map[string]string{"Depth": "0"},
	)
	require.Contains(
		t,
		string(requireWebDAVStatus(t, response, http.StatusMultiStatus)),
		"blue",
	)

	s3URL := environment.server.URL + "/hackmd/shared.txt"
	response = doWebDAVRequest(
		t,
		client,
		"editor",
		"secret",
		http.MethodPut,
		s3URL,
		strings.NewReader("s3-one"),
		nil,
	)
	requireWebDAVStatus(t, response, http.StatusOK)
	s3DAVURL := environment.server.URL + "/webdav/hackmd/shared.txt"
	response = doWebDAVRequest(
		t,
		client,
		"editor",
		"secret",
		"PROPPATCH",
		s3DAVURL,
		strings.NewReader(propertyUpdate),
		map[string]string{"Content-Type": "application/xml"},
	)
	requireWebDAVStatus(t, response, http.StatusMultiStatus)
	response = doWebDAVRequest(
		t,
		client,
		"editor",
		"secret",
		http.MethodPut,
		s3URL,
		strings.NewReader("s3-two"),
		nil,
	)
	requireWebDAVStatus(t, response, http.StatusOK)
	response = doWebDAVRequest(
		t,
		client,
		"editor",
		"secret",
		"PROPFIND",
		s3DAVURL,
		strings.NewReader(propfind),
		map[string]string{"Depth": "0"},
	)
	require.Contains(
		t,
		string(requireWebDAVStatus(t, response, http.StatusMultiStatus)),
		"blue",
	)
}

func TestWebDAVPersistentLocksAndLockNullCleanup(t *testing.T) {
	environment := newWebDAVIntegrationEnvironment(
		t,
		map[string]string{"editor": "secret"},
		server.WebDAVOptions{},
		8*1024*1024,
	)
	client := environment.server.Client()
	root := environment.server.URL + "/webdav/locks"
	fileURL := root + "/file.txt"
	requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", "MKCOL", root, nil, nil,
	), http.StatusCreated)
	requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", http.MethodPut, fileURL, strings.NewReader("old"), nil,
	), http.StatusCreated)

	lockBody := `<D:lockinfo xmlns:D="DAV:"><D:lockscope><D:exclusive/>` +
		`</D:lockscope><D:locktype><D:write/></D:locktype>` +
		`<D:owner><D:href>editor</D:href></D:owner></D:lockinfo>`
	response := doWebDAVRequest(
		t,
		client,
		"editor",
		"secret",
		"LOCK",
		fileURL,
		strings.NewReader(lockBody),
		map[string]string{"Depth": "0", "Timeout": "Second-600"},
	)
	requireWebDAVStatus(t, response, http.StatusOK)
	lockTokenHeader := response.Header.Get("Lock-Token")
	require.Regexp(t, `^<opaquelocktoken:[^>]+>$`, lockTokenHeader)
	lockToken := strings.TrimSuffix(strings.TrimPrefix(lockTokenHeader, "<"), ">")

	response = doWebDAVRequest(
		t,
		client,
		"editor",
		"secret",
		http.MethodPut,
		fileURL,
		strings.NewReader("blocked"),
		nil,
	)
	requireWebDAVStatus(t, response, http.StatusLocked)
	response = doWebDAVRequest(
		t,
		client,
		"editor",
		"secret",
		http.MethodPut,
		fileURL,
		strings.NewReader("updated"),
		map[string]string{"If": "(<" + lockToken + ">)"},
	)
	requireWebDAVStatus(t, response, http.StatusNoContent)

	response = doWebDAVRequest(
		t,
		client,
		"editor",
		"secret",
		"LOCK",
		fileURL,
		nil,
		map[string]string{
			"If":      "(<" + lockToken + ">)",
			"Timeout": "Second-1200",
		},
	)
	requireWebDAVStatus(t, response, http.StatusOK)
	require.Equal(t, lockTokenHeader, response.Header.Get("Lock-Token"))

	restarted, err := server.New(
		"127.0.0.1:0",
		server.WithUser(map[string]string{"editor": "secret"}),
		server.WithWebDAV(server.WebDAVOptions{Enabled: true, Root: "/"}),
		server.WithFileManager(environment.manager),
	)
	require.NoError(t, err)
	restartedServer := httptest.NewServer(restarted)
	t.Cleanup(restartedServer.Close)
	response = doWebDAVRequest(
		t,
		restartedServer.Client(),
		"editor",
		"secret",
		http.MethodPut,
		restartedServer.URL+"/webdav/locks/file.txt",
		strings.NewReader("still-blocked"),
		nil,
	)
	requireWebDAVStatus(t, response, http.StatusLocked)

	response = doWebDAVRequest(
		t,
		client,
		"editor",
		"secret",
		"UNLOCK",
		fileURL,
		nil,
		map[string]string{"Lock-Token": lockTokenHeader},
	)
	requireWebDAVStatus(t, response, http.StatusNoContent)
	response = doWebDAVRequest(
		t,
		client,
		"editor",
		"secret",
		http.MethodPut,
		fileURL,
		strings.NewReader("unlocked"),
		nil,
	)
	requireWebDAVStatus(t, response, http.StatusNoContent)

	lockNullURL := root + "/new.txt"
	response = doWebDAVRequest(
		t,
		client,
		"editor",
		"secret",
		"LOCK",
		lockNullURL,
		strings.NewReader(lockBody),
		map[string]string{"Depth": "0"},
	)
	requireWebDAVStatus(t, response, http.StatusCreated)
	lockNullToken := response.Header.Get("Lock-Token")
	response = doWebDAVRequest(
		t,
		client,
		"editor",
		"secret",
		http.MethodHead,
		lockNullURL,
		nil,
		nil,
	)
	requireWebDAVStatus(t, response, http.StatusOK)
	require.Equal(t, "0", response.Header.Get("Content-Length"))
	response = doWebDAVRequest(
		t,
		client,
		"editor",
		"secret",
		"UNLOCK",
		lockNullURL,
		nil,
		map[string]string{"Lock-Token": lockNullToken},
	)
	requireWebDAVStatus(t, response, http.StatusNoContent)
	response = doWebDAVRequest(
		t,
		client,
		"editor",
		"secret",
		http.MethodHead,
		lockNullURL,
		nil,
		nil,
	)
	requireWebDAVStatus(t, response, http.StatusNotFound)
}

func TestWebDAVChunkedPermissionsQuotaDestinationAndSync(t *testing.T) {
	tempDir := t.TempDir()
	staleSpool := filepath.Join(tempDir, "tgfile-webdav-stale")
	require.NoError(t, os.WriteFile(staleSpool, []byte("stale"), 0o600))
	staleTime := time.Now().Add(-25 * time.Hour)
	require.NoError(t, os.Chtimes(staleSpool, staleTime, staleTime))
	const uploadSize = 6 * 1024 * 1024
	environment := newWebDAVIntegrationEnvironment(
		t,
		map[string]string{"editor": "secret", "reader": "read-secret"},
		server.WebDAVOptions{
			MaxUploadSize:      8 * 1024 * 1024,
			UploadTempDir:      tempDir,
			Users:              map[string]string{"editor": "read-write", "reader": "read"},
			QuotaBytes:         uploadSize,
			MaxMutationEntries: 100,
			SyncPageSize:       100,
		},
		8*1024*1024,
	)
	require.NoFileExists(t, staleSpool)
	client := environment.server.Client()
	root := environment.server.URL + "/webdav/sync"
	requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", "MKCOL", root, nil, nil,
	), http.StatusCreated)
	readOnlyUnknown := doWebDAVRequest(
		t, client, "reader", "read-secret", "PATCH", root, nil, nil,
	)
	requireWebDAVStatus(t, readOnlyUnknown, http.StatusMethodNotAllowed)
	require.NotContains(t, readOnlyUnknown.Header.Get("Allow"), "PUT")

	chunkedBody := &unknownLengthReader{reader: bytes.NewReader(bytes.Repeat([]byte("x"), uploadSize))}
	response := doWebDAVRequest(
		t,
		client,
		"editor",
		"secret",
		http.MethodPut,
		root+"/large.bin",
		chunkedBody,
		nil,
	)
	requireWebDAVStatus(t, response, http.StatusCreated)
	entries, err := os.ReadDir(tempDir)
	require.NoError(t, err)
	require.Empty(t, entries)

	response = doWebDAVRequest(
		t,
		client,
		"reader",
		"read-secret",
		http.MethodGet,
		root+"/large.bin",
		nil,
		map[string]string{"Range": "bytes=6291450-6291455"},
	)
	require.Equal(t, []byte("xxxxxx"), requireWebDAVStatus(t, response, http.StatusPartialContent))
	response = doWebDAVRequest(
		t,
		client,
		"reader",
		"read-secret",
		http.MethodPut,
		root+"/denied.txt",
		strings.NewReader("denied"),
		nil,
	)
	requireWebDAVStatus(t, response, http.StatusForbidden)

	response = doWebDAVRequest(
		t,
		client,
		"editor",
		"secret",
		http.MethodPut,
		root+"/over-quota.txt",
		strings.NewReader("x"),
		nil,
	)
	requireWebDAVStatus(t, response, http.StatusInsufficientStorage)

	response = doWebDAVRequest(
		t,
		client,
		"editor",
		"secret",
		"COPY",
		root+"/large.bin",
		nil,
		map[string]string{
			"Destination": "https://other.invalid/webdav/sync/copy.bin",
		},
	)
	requireWebDAVStatus(t, response, http.StatusBadGateway)
	response = doWebDAVRequest(
		t,
		client,
		"editor",
		"secret",
		"COPY",
		root+"/large.bin",
		nil,
		map[string]string{
			"Destination": environment.server.URL + "/webdav2/sync/copy.bin",
		},
	)
	requireWebDAVStatus(t, response, http.StatusBadRequest)

	response = doWebDAVRequest(
		t,
		client,
		"editor",
		"secret",
		"PROPFIND",
		root,
		nil,
		nil,
	)
	require.Contains(
		t,
		string(requireWebDAVStatus(t, response, http.StatusForbidden)),
		"propfind-finite-depth",
	)

	reportBody := `<D:sync-collection xmlns:D="DAV:"><D:sync-token/>` +
		`<D:sync-level>1</D:sync-level><D:prop><D:getetag/></D:prop>` +
		`</D:sync-collection>`
	response = doWebDAVRequest(
		t,
		client,
		"editor",
		"secret",
		"REPORT",
		root,
		strings.NewReader(reportBody),
		map[string]string{"Depth": "0"},
	)
	body := requireWebDAVStatus(t, response, http.StatusMultiStatus)
	require.Contains(t, string(body), "large.bin")
	require.Contains(t, string(body), "urn:tgfile:webdav-sync:")
}

type unknownLengthReader struct {
	reader io.Reader
}

func (r *unknownLengthReader) Read(buffer []byte) (int, error) {
	return r.reader.Read(buffer)
}
