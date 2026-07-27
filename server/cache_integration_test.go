package server_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/xxxsen/common/database"

	"github.com/xxxsen/tgfile/blockio"
	"github.com/xxxsen/tgfile/db"
	"github.com/xxxsen/tgfile/filemgr"
	"github.com/xxxsen/tgfile/server"
	"github.com/xxxsen/tgfile/utils"
)

const cacheIntegrationBlockSize = 8 * 1024 * 1024

type cacheIntegrationBlockIO struct {
	mu            sync.Mutex
	parts         map[string][]byte
	nextPart      int
	downloads     int
	deletes       int
	downloadGate  <-chan struct{}
	downloadStart chan struct{}
}

func newCacheIntegrationBlockIO() *cacheIntegrationBlockIO {
	return &cacheIntegrationBlockIO{
		parts:         make(map[string][]byte),
		downloadStart: make(chan struct{}, 32),
	}
}

func (b *cacheIntegrationBlockIO) Name() string {
	return "cache-integration"
}

func (b *cacheIntegrationBlockIO) MaxFileSize() int64 {
	return cacheIntegrationBlockSize
}

func (b *cacheIntegrationBlockIO) Upload(_ context.Context, reader io.Reader) (*blockio.UploadResult, error) {
	raw, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("read cache integration upload: %w", err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	key := fmt.Sprintf("cache-part-%d", b.nextPart)
	b.nextPart++
	b.parts[key] = bytes.Clone(raw)
	return &blockio.UploadResult{
		FileKey:    key,
		DeleteRef:  key,
		UploadedAt: time.Now().UnixMilli(),
	}, nil
}

func (b *cacheIntegrationBlockIO) Download(
	ctx context.Context,
	key string,
	position int64,
) (io.ReadCloser, error) {
	b.mu.Lock()
	raw, found := b.parts[key]
	b.downloads++
	gate := b.downloadGate
	b.mu.Unlock()
	if !found {
		return nil, fmt.Errorf("cache integration block %q not found", key)
	}
	select {
	case b.downloadStart <- struct{}{}:
	default:
	}
	if gate != nil {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("wait for cache integration download: %w", ctx.Err())
		case <-gate:
		}
	}
	if position < 0 || position > int64(len(raw)) {
		return nil, fmt.Errorf("invalid cache integration block offset %d", position)
	}
	return io.NopCloser(bytes.NewReader(bytes.Clone(raw[position:]))), nil
}

func (b *cacheIntegrationBlockIO) DeleteBlocks(_ context.Context, refs []string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.deletes += len(refs)
	for _, ref := range refs {
		delete(b.parts, ref)
	}
	return nil
}

func (b *cacheIntegrationBlockIO) setDownloadGate(gate <-chan struct{}) {
	b.mu.Lock()
	b.downloadGate = gate
	b.mu.Unlock()
}

func (b *cacheIntegrationBlockIO) resetDownloads() {
	b.mu.Lock()
	b.downloads = 0
	b.mu.Unlock()
	for {
		select {
		case <-b.downloadStart:
		default:
			return
		}
	}
}

func (b *cacheIntegrationBlockIO) counts() (int, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.downloads, b.deletes
}

type signalingFileManager struct {
	filemgr.IFileManager
	openCalls chan struct{}
	openDone  chan struct{}
}

func (m *signalingFileManager) OpenFile(ctx context.Context, fileID uint64) (io.ReadSeekCloser, error) {
	m.openCalls <- struct{}{}
	reader, err := m.IFileManager.OpenFile(ctx, fileID)
	m.openDone <- struct{}{}
	return reader, err
}

type cacheIntegrationEnvironment struct {
	server    *httptest.Server
	database  database.IDatabase
	manager   filemgr.IFileManager
	cacheDir  string
	block     *cacheIntegrationBlockIO
	openCalls <-chan struct{}
	openDone  <-chan struct{}
}

func newCacheIntegrationEnvironment(t *testing.T) *cacheIntegrationEnvironment {
	t.Helper()
	root := t.TempDir()
	databaseClient, err := db.Open(filepath.Join(root, "cache.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, databaseClient.Close()) })
	cacheDir := filepath.Join(root, "cache")
	cache, err := filemgr.NewFileIOCache(&filemgr.FileIOCacheConfig{
		L1CacheSize:    64,
		L1KeySizeLimit: 16,
		L2CacheSize:    16 * 1024 * 1024,
		L2KeySizeLimit: 8 * 1024 * 1024,
		L2CacheDir:     cacheDir,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		closeContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, cache.Close(closeContext))
	})
	block := newCacheIntegrationBlockIO()
	manager := filemgr.NewFileManager(databaseClient, block, cache)
	openCalls := make(chan struct{}, 32)
	openDone := make(chan struct{}, 32)
	signaling := &signalingFileManager{
		IFileManager: manager,
		openCalls:    openCalls,
		openDone:     openDone,
	}
	handler, err := server.New(
		"127.0.0.1:0",
		server.WithS3(server.S3Options{
			Enabled: true,
			Buckets: []server.S3BucketOptions{{Name: "hackmd", ACL: server.BucketACLPublicRead}},
		}),
		server.WithUser(map[string]string{"access": "secret"}),
		server.WithEnableWebdav(true, "/"),
		server.WithFileManager(signaling),
	)
	require.NoError(t, err)
	testServer := httptest.NewServer(handler)
	t.Cleanup(testServer.Close)
	return &cacheIntegrationEnvironment{
		server:    testServer,
		database:  databaseClient,
		manager:   manager,
		cacheDir:  cacheDir,
		block:     block,
		openCalls: openCalls,
		openDone:  openDone,
	}
}

func TestCacheAcrossS3DirectLinkAndWebDAV(t *testing.T) {
	environment := newCacheIntegrationEnvironment(t)
	content := []byte("cache protocol integration content exceeds L1")
	objectPath := "/hackmd/cache-protocol.bin"
	objectURL := environment.server.URL + objectPath
	response, err := environment.server.Client().Do(authenticatedRequest(
		t,
		http.MethodPut,
		objectURL,
		bytes.NewReader(content),
	))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	_ = readResponse(t, response)
	object, err := environment.manager.StatS3Object(t.Context(), objectPath)
	require.NoError(t, err)
	fileKey := cacheIntegrationFileKey(object.Link.FileId)
	require.NoError(t, environment.manager.CreateFileLink(
		t.Context(),
		"/defaults/"+fileKey[:2]+"/"+fileKey,
		object.Link.FileId,
		0,
		false,
	))
	_, err = environment.manager.PublishWebDAVFile(
		t.Context(),
		"/cache-protocol.bin",
		object.Link.FileId,
		int64(len(content)),
		filemgr.WebDAVMutationOptions{Principal: "access"},
	)
	require.NoError(t, err)

	baseline := cacheBusinessCounts(t, environment.database)
	environment.block.resetDownloads()
	gate := make(chan struct{})
	environment.block.setDownloadGate(gate)
	requests := []cacheProtocolRequest{
		{target: objectURL, status: http.StatusOK, body: content},
		{target: objectURL, status: http.StatusPartialContent, body: content[3:12], rangeHeader: "bytes=3-11"},
		{target: environment.server.URL + "/file/download/" + fileKey, status: http.StatusOK, body: content},
		{
			target:   environment.server.URL + "/webdav/cache-protocol.bin",
			status:   http.StatusOK,
			body:     content,
			username: "access",
			password: "secret",
		},
	}
	start := make(chan struct{})
	results := make(chan error, len(requests))
	for _, request := range requests {
		go func() {
			<-start
			results <- executeCacheProtocolRequest(t.Context(), environment.server.Client(), request)
		}()
	}
	close(start)
	for range requests {
		requireSignalWithin(t, environment.openCalls)
	}
	requireSignalWithin(t, environment.block.downloadStart)
	close(gate)
	for range requests {
		require.NoError(t, requireSignalWithin(t, results))
	}
	environment.block.setDownloadGate(nil)
	downloads, deletes := environment.block.counts()
	require.Equal(t, 1, downloads)
	require.Zero(t, deletes)
	require.Equal(t, baseline, cacheBusinessCounts(t, environment.database))

	recoveryRequests := []struct {
		name    string
		request cacheProtocolRequest
	}{
		{name: "S3", request: cacheProtocolRequest{target: objectURL, status: http.StatusOK, body: content}},
		{
			name: "direct link",
			request: cacheProtocolRequest{
				target: environment.server.URL + "/file/download/" + fileKey,
				status: http.StatusOK,
				body:   content,
			},
		},
		{
			name: "WebDAV",
			request: cacheProtocolRequest{
				target:   environment.server.URL + "/webdav/cache-protocol.bin",
				status:   http.StatusOK,
				body:     content,
				username: "access",
				password: "secret",
			},
		},
	}
	for _, recovery := range recoveryRequests {
		t.Run(recovery.name+" corruption recovery", func(t *testing.T) {
			cacheFiles := managedCacheFiles(t, environment.cacheDir)
			require.Len(t, cacheFiles, 1)
			require.NoError(t, os.WriteFile(cacheFiles[0], []byte("corrupt"), 0o600))
			environment.block.resetDownloads()
			require.NoError(t, executeCacheProtocolRequest(
				t.Context(),
				environment.server.Client(),
				recovery.request,
			))
			downloads, deletes = environment.block.counts()
			require.Equal(t, 1, downloads)
			require.Zero(t, deletes)
			require.Equal(t, baseline, cacheBusinessCounts(t, environment.database))
			require.Empty(t, managedCacheTemps(t, environment.cacheDir))
		})
	}

	head, err := http.NewRequestWithContext(t.Context(), http.MethodHead, objectURL, nil)
	require.NoError(t, err)
	response, err = environment.server.Client().Do(head)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, fmt.Sprintf("%d", len(content)), response.Header.Get("Content-Length"))
	_ = readResponse(t, response)
}

func TestCacheHandlesCompositeZeroBoundaryAndCanceledProtocolReads(t *testing.T) {
	environment := newCacheIntegrationEnvironment(t)
	client := environment.server.Client()

	zeroURL := environment.server.URL + "/hackmd/cache-zero.bin"
	response, err := client.Do(authenticatedRequest(t, http.MethodPut, zeroURL, bytes.NewReader(nil)))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	_ = readResponse(t, response)
	environment.block.resetDownloads()
	require.NoError(t, executeCacheProtocolRequest(t.Context(), client, cacheProtocolRequest{
		target: zeroURL,
		status: http.StatusOK,
		body:   []byte{},
	}))
	downloads, _ := environment.block.counts()
	require.Zero(t, downloads)

	boundary := []byte("1234567890abcdef")
	boundaryURL := environment.server.URL + "/hackmd/cache-boundary.bin"
	response, err = client.Do(authenticatedRequest(t, http.MethodPut, boundaryURL, bytes.NewReader(boundary)))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	_ = readResponse(t, response)
	environment.block.resetDownloads()
	for range 2 {
		require.NoError(t, executeCacheProtocolRequest(t.Context(), client, cacheProtocolRequest{
			target: boundaryURL,
			status: http.StatusOK,
			body:   boundary,
		}))
	}
	downloads, _ = environment.block.counts()
	require.Equal(t, 1, downloads)

	compositeURL := environment.server.URL + "/hackmd/cache-composite.bin"
	firstContent := bytes.Repeat([]byte("m"), 5*1024*1024)
	secondContent := []byte("composite-tail")
	allContent := append(bytes.Clone(firstContent), secondContent...)
	uploadID := initiateCacheMultipart(t, client, compositeURL)
	firstETag := uploadIntegrationPart(t, client, compositeURL, uploadID, 1, firstContent)
	secondETag := uploadIntegrationPart(t, client, compositeURL, uploadID, 2, secondContent)
	completeCacheMultipart(t, client, compositeURL, uploadID, firstETag, secondETag)
	environment.block.resetDownloads()
	require.NoError(t, executeCacheProtocolRequest(t.Context(), client, cacheProtocolRequest{
		target: compositeURL,
		status: http.StatusOK,
		body:   allContent,
	}))
	downloads, _ = environment.block.counts()
	require.Equal(t, 2, downloads)
	require.NoError(t, executeCacheProtocolRequest(t.Context(), client, cacheProtocolRequest{
		target:   environment.server.URL + "/webdav/hackmd/cache-composite.bin",
		status:   http.StatusOK,
		body:     allContent,
		username: "access",
		password: "secret",
	}))
	downloads, _ = environment.block.counts()
	require.Equal(t, 2, downloads)

	cancelContent := bytes.Repeat([]byte("c"), 1024)
	cancelURL := environment.server.URL + "/hackmd/cache-cancel.bin"
	response, err = client.Do(authenticatedRequest(t, http.MethodPut, cancelURL, bytes.NewReader(cancelContent)))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	_ = readResponse(t, response)
	environment.block.resetDownloads()
	cancelGate := make(chan struct{})
	environment.block.setDownloadGate(cancelGate)
	drainCacheSignals(environment.openDone)
	requestContext, cancelRequest := context.WithCancel(t.Context())
	request, err := http.NewRequestWithContext(requestContext, http.MethodGet, cancelURL, nil)
	require.NoError(t, err)
	canceledResult := make(chan error, 1)
	go func() {
		response, requestErr := client.Do(request)
		if response != nil {
			_ = response.Body.Close()
		}
		canceledResult <- requestErr
	}()
	requireSignalWithin(t, environment.block.downloadStart)
	cancelRequest()
	require.ErrorIs(t, requireSignalWithin(t, canceledResult), context.Canceled)
	requireSignalWithin(t, environment.openDone)
	environment.block.setDownloadGate(nil)
	require.Empty(t, managedCacheTemps(t, environment.cacheDir))
	require.NoError(t, executeCacheProtocolRequest(t.Context(), client, cacheProtocolRequest{
		target: cancelURL,
		status: http.StatusOK,
		body:   cancelContent,
	}))
	_, deletes := environment.block.counts()
	require.Zero(t, deletes)
}

type cacheProtocolRequest struct {
	target      string
	status      int
	body        []byte
	rangeHeader string
	username    string
	password    string
}

func executeCacheProtocolRequest(
	ctx context.Context,
	client *http.Client,
	expected cacheProtocolRequest,
) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, expected.target, nil)
	if err != nil {
		return fmt.Errorf("create cache protocol request: %w", err)
	}
	if expected.rangeHeader != "" {
		request.Header.Set("Range", expected.rangeHeader)
	}
	if expected.username != "" {
		request.SetBasicAuth(expected.username, expected.password)
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("execute cache protocol request: %w", err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		return fmt.Errorf("read cache protocol response: %w", err)
	}
	if response.StatusCode != expected.status {
		return fmt.Errorf("cache protocol status: got %d, want %d", response.StatusCode, expected.status)
	}
	if !bytes.Equal(raw, expected.body) {
		return fmt.Errorf("cache protocol body mismatch: got %d bytes, want %d", len(raw), len(expected.body))
	}
	return nil
}

func cacheIntegrationFileKey(fileID uint64) string {
	hash := hex.EncodeToString(utils.FileIdToHash(fileID))
	return hash + "-cache.bin"
}

type cacheTableCounts struct {
	files          int
	parts          int
	mappings       int
	deleteState    int
	deleteNonLive  int
	deleteAttempts int
}

func cacheBusinessCounts(t *testing.T, databaseClient database.IDatabase) cacheTableCounts {
	t.Helper()
	return cacheTableCounts{
		files:       queryIntegrationCount(t, databaseClient, "SELECT COUNT(*) FROM tg_file_tab"),
		parts:       queryIntegrationCount(t, databaseClient, "SELECT COUNT(*) FROM tg_file_part_tab"),
		mappings:    queryIntegrationCount(t, databaseClient, "SELECT COUNT(*) FROM tg_file_mapping_tab"),
		deleteState: queryIntegrationCount(t, databaseClient, "SELECT COUNT(*) FROM tg_file_part_delete_state_tab"),
		deleteNonLive: queryIntegrationCount(
			t,
			databaseClient,
			"SELECT COUNT(*) FROM tg_file_part_delete_state_tab WHERE delete_state <> 'live'",
		),
		deleteAttempts: queryIntegrationCount(
			t,
			databaseClient,
			"SELECT COALESCE(SUM(attempt_count), 0) FROM tg_file_part_delete_state_tab",
		),
	}
}

func managedCacheFiles(t *testing.T, cacheDir string) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(cacheDir, "v2", "*", "*.cache"))
	require.NoError(t, err)
	return paths
}

func managedCacheTemps(t *testing.T, cacheDir string) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(cacheDir, "v2", "*", "*.temp"))
	require.NoError(t, err)
	return paths
}

func requireSignalWithin[T any](t *testing.T, channel <-chan T) T {
	t.Helper()
	select {
	case value := <-channel:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for cache integration event")
		var zero T
		return zero
	}
}

func drainCacheSignals(channel <-chan struct{}) {
	for {
		select {
		case <-channel:
		default:
			return
		}
	}
}

func initiateCacheMultipart(t *testing.T, client *http.Client, objectURL string) string {
	t.Helper()
	response, err := client.Do(authenticatedRequest(t, http.MethodPost, objectURL+"?uploads", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	var initiated struct {
		UploadID string `xml:"UploadId"`
	}
	require.NoError(t, xml.Unmarshal(readResponse(t, response), &initiated))
	require.NotEmpty(t, initiated.UploadID)
	return initiated.UploadID
}

func completeCacheMultipart(
	t *testing.T,
	client *http.Client,
	objectURL, uploadID, firstETag, secondETag string,
) {
	t.Helper()
	body := []byte(
		"<CompleteMultipartUpload>" +
			"<Part><PartNumber>1</PartNumber><ETag>" + firstETag + "</ETag></Part>" +
			"<Part><PartNumber>2</PartNumber><ETag>" + secondETag + "</ETag></Part>" +
			"</CompleteMultipartUpload>",
	)
	response, err := client.Do(authenticatedRequest(
		t,
		http.MethodPost,
		objectURL+"?uploadId="+uploadID,
		bytes.NewReader(body),
	))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	_ = readResponse(t, response)
}
