package server_test

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/xxxsen/common/logger"

	"github.com/xxxsen/tgfile/blockio/mem"
	"github.com/xxxsen/tgfile/db"
	"github.com/xxxsen/tgfile/filemgr"
	"github.com/xxxsen/tgfile/server"
)

func newIntegrationServer(t *testing.T) *httptest.Server {
	t.Helper()
	logger.Init("", "debug", 0, 0, 0, true)
	database, err := db.Open(filepath.Join(t.TempDir(), "data.db"))
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, database.Close())
	})
	block, err := mem.New(4)
	require.NoError(t, err)
	cache, err := filemgr.NewFileIOCache(&filemgr.FileIOCacheConfig{
		DisableL1Cache: true,
		DisableL2Cache: true,
	})
	require.NoError(t, err)
	manager := filemgr.NewFileManager(database, block, cache)
	handler, err := server.New(
		"127.0.0.1:0",
		server.WithEnableS3(true, []string{"hackmd"}),
		server.WithUser(map[string]string{"access": "secret"}),
		server.WithFileManager(manager),
	)
	require.NoError(t, err)
	testServer := httptest.NewServer(handler)
	t.Cleanup(testServer.Close)
	return testServer
}

func authenticatedRequest(t *testing.T, method, target string, body io.Reader) *http.Request {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), method, target, body)
	require.NoError(t, err)
	request.SetBasicAuth("access", "secret")
	return request
}

func getResponse(t *testing.T, client *http.Client, target string) (*http.Response, error) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	return client.Do(request)
}

func readResponse(t *testing.T, response *http.Response) []byte {
	t.Helper()
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	return raw
}

func TestS3PutGetHeadRangeAndConflict(t *testing.T) {
	testServer := newIntegrationServer(t)
	client := testServer.Client()
	content := []byte("0123456789")
	objectURL := testServer.URL + "/hackmd/reports/data.bin"

	response, err := client.Do(authenticatedRequest(t, http.MethodPut, objectURL, bytes.NewReader(content)))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	_ = readResponse(t, response)

	response, err = getResponse(t, client, objectURL)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, content, readResponse(t, response))

	request, err := http.NewRequestWithContext(t.Context(), http.MethodHead, objectURL, nil)
	require.NoError(t, err)
	response, err = client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, "10", response.Header.Get("Content-Length"))
	require.Equal(t, "bytes", response.Header.Get("Accept-Ranges"))
	require.Empty(t, readResponse(t, response))

	request, err = http.NewRequestWithContext(t.Context(), http.MethodGet, objectURL, nil)
	require.NoError(t, err)
	request.Header.Set("Range", "bytes=3-7")
	response, err = client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusPartialContent, response.StatusCode)
	require.Equal(t, []byte("34567"), readResponse(t, response))

	response, err = client.Do(authenticatedRequest(t, http.MethodPut, objectURL, bytes.NewReader([]byte("replacement"))))
	require.NoError(t, err)
	require.Equal(t, http.StatusConflict, response.StatusCode)
	_ = readResponse(t, response)

	response, err = getResponse(t, client, objectURL)
	require.NoError(t, err)
	require.Equal(t, content, readResponse(t, response))
}

func TestDirectUploadDownload(t *testing.T) {
	testServer := newIntegrationServer(t)
	client := testServer.Client()
	content := []byte("direct upload content")

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "报告.txt")
	require.NoError(t, err)
	_, err = part.Write(content)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	request := authenticatedRequest(t, http.MethodPost, testServer.URL+"/file/upload", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response, err := client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	var uploadResponse struct {
		Code uint32 `json:"code"`
		Data struct {
			Key string `json:"key"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(readResponse(t, response), &uploadResponse))
	require.Zero(t, uploadResponse.Code)
	require.NotEmpty(t, uploadResponse.Data.Key)

	response, err = getResponse(t, client, testServer.URL+"/file/download/"+uploadResponse.Data.Key)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, content, readResponse(t, response))

	response, err = getResponse(t, client, testServer.URL+"/file/download/a-x")
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, response.StatusCode)
	_ = readResponse(t, response)
}
