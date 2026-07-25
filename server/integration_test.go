package server_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsv4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/stretchr/testify/require"
	"github.com/xxxsen/common/database"
	"github.com/xxxsen/common/logger"

	"github.com/xxxsen/tgfile/blockio/mem"
	"github.com/xxxsen/tgfile/db"
	"github.com/xxxsen/tgfile/filemgr"
	"github.com/xxxsen/tgfile/s3checksum"
	"github.com/xxxsen/tgfile/server"
)

type integrationEnvironment struct {
	server   *httptest.Server
	database database.IDatabase
	manager  filemgr.IFileManager
}

func newIntegrationEnvironment(t *testing.T) *integrationEnvironment {
	t.Helper()
	logger.Init("", "debug", 0, 0, 0, true)
	database, err := db.Open(filepath.Join(t.TempDir(), "data.db"))
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, database.Close())
	})
	block, err := mem.New(8 * 1024 * 1024)
	require.NoError(t, err)
	cache, err := filemgr.NewFileIOCache(&filemgr.FileIOCacheConfig{
		DisableL1Cache: true,
		DisableL2Cache: true,
	})
	require.NoError(t, err)
	manager := filemgr.NewFileManager(database, block, cache)
	handler, err := server.New(
		"127.0.0.1:0",
		server.WithS3(server.S3Options{
			Enabled: true,
			Buckets: []server.S3BucketOptions{{
				Name: "hackmd",
				ACL:  server.BucketACLPublicRead,
			}, {
				Name: "private-data",
				ACL:  server.BucketACLPrivate,
			}},
		}),
		server.WithUser(map[string]string{"access": "secret"}),
		server.WithEnableWebdav(true, "/"),
		server.WithFileManager(manager),
	)
	require.NoError(t, err)
	testServer := httptest.NewServer(handler)
	t.Cleanup(testServer.Close)
	return &integrationEnvironment{
		server:   testServer,
		database: database,
		manager:  manager,
	}
}

func newIntegrationServer(t *testing.T) *httptest.Server {
	t.Helper()
	return newIntegrationEnvironment(t).server
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

type listObjectsV1Response struct {
	Name           string                 `xml:"Name"`
	Prefix         string                 `xml:"Prefix"`
	Marker         string                 `xml:"Marker"`
	NextMarker     string                 `xml:"NextMarker"`
	MaxKeys        int                    `xml:"MaxKeys"`
	Delimiter      string                 `xml:"Delimiter"`
	IsTruncated    bool                   `xml:"IsTruncated"`
	EncodingType   string                 `xml:"EncodingType"`
	Contents       []listObjectsV1Content `xml:"Contents"`
	CommonPrefixes []struct {
		Prefix string `xml:"Prefix"`
	} `xml:"CommonPrefixes"`
}

type listObjectsV1Content struct {
	Key          string `xml:"Key"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
	StorageClass string `xml:"StorageClass"`
	Owner        *struct {
		ID          string `xml:"ID"`
		DisplayName string `xml:"DisplayName"`
	} `xml:"Owner"`
}

func decodeListObjectsV1(t *testing.T, raw []byte) listObjectsV1Response {
	t.Helper()
	var result listObjectsV1Response
	require.NoError(t, xml.Unmarshal(raw, &result))
	return result
}

func TestS3PutGetHeadRangeAndOverwrite(t *testing.T) {
	testServer := newIntegrationServer(t)
	client := testServer.Client()
	content := []byte("0123456789")
	objectURL := testServer.URL + "/hackmd/reports/data.bin"

	response, err := client.Do(authenticatedRequest(t, http.MethodPut, objectURL, bytes.NewReader(content)))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.NotEmpty(t, response.Header.Get("X-Amz-Request-Id"))
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
	require.Equal(t, http.StatusOK, response.StatusCode)
	_ = readResponse(t, response)

	response, err = getResponse(t, client, objectURL)
	require.NoError(t, err)
	require.Equal(t, []byte("replacement"), readResponse(t, response))
}

func TestS3ConditionsChecksumsAndRangeErrors(t *testing.T) {
	testServer := newIntegrationServer(t)
	client := testServer.Client()
	content := []byte("condition-content")
	objectURL := testServer.URL + "/hackmd/conditions/object.bin"

	request := authenticatedRequest(t, http.MethodPut, objectURL, bytes.NewReader(content))
	request.Header.Set("Content-MD5", "FPmkKPkIIGQ8gujCaiPjGQ==")
	request.Header.Set("X-Amz-Checksum-Sha256", "VhtMrotqbDFU8IcjQZ/osL6hEejuQrOuAg4v6fqpkP8=")
	response, err := client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	etag := response.Header.Get("ETag")
	require.NotEmpty(t, etag)
	_ = readResponse(t, response)

	request, err = http.NewRequestWithContext(t.Context(), http.MethodGet, objectURL, nil)
	require.NoError(t, err)
	request.Header.Set("If-None-Match", etag)
	response, err = client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotModified, response.StatusCode)
	require.Empty(t, readResponse(t, response))

	request, err = http.NewRequestWithContext(t.Context(), http.MethodGet, objectURL, nil)
	require.NoError(t, err)
	request.Header.Set("If-Match", `"different"`)
	response, err = client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusPreconditionFailed, response.StatusCode)
	_ = readResponse(t, response)

	request, err = http.NewRequestWithContext(t.Context(), http.MethodGet, objectURL, nil)
	require.NoError(t, err)
	request.Header.Set("If-None-Match", `"unterminated`)
	response, err = client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, response.StatusCode)
	require.Contains(t, string(readResponse(t, response)), "InvalidArgument")

	request, err = http.NewRequestWithContext(t.Context(), http.MethodGet, objectURL, nil)
	require.NoError(t, err)
	request.Header.Set("Range", "bytes=999-1000")
	response, err = client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusRequestedRangeNotSatisfiable, response.StatusCode)
	require.Contains(t, string(readResponse(t, response)), "InvalidRange")

	request, err = http.NewRequestWithContext(t.Context(), http.MethodGet, objectURL, nil)
	require.NoError(t, err)
	request.Header.Set("Range", "bytes=0-3")
	request.Header.Set("If-Range", etag)
	response, err = client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusPartialContent, response.StatusCode)
	require.Empty(t, response.Header.Get("X-Amz-Checksum-Sha256"))
	require.Empty(t, response.Header.Get("X-Amz-Checksum-Type"))
	require.Equal(t, []byte("cond"), readResponse(t, response))

	request, err = http.NewRequestWithContext(t.Context(), http.MethodGet, objectURL, nil)
	require.NoError(t, err)
	request.Header.Set("Range", "bytes=0-3")
	request.Header.Set("If-Range", `"different"`)
	response, err = client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, "VhtMrotqbDFU8IcjQZ/osL6hEejuQrOuAg4v6fqpkP8=", response.Header.Get("X-Amz-Checksum-Sha256"))
	require.Equal(t, content, readResponse(t, response))

	request = authenticatedRequest(t, http.MethodPut, objectURL, bytes.NewReader([]byte("replacement")))
	request.Header.Set("If-None-Match", "*")
	response, err = client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusPreconditionFailed, response.StatusCode)
	_ = readResponse(t, response)

	badURL := testServer.URL + "/hackmd/conditions/bad.bin"
	request = authenticatedRequest(t, http.MethodPut, badURL, bytes.NewReader(content))
	request.Header.Set("X-Amz-Checksum-Sha256", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	response, err = client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, response.StatusCode)
	require.Contains(t, string(readResponse(t, response)), "BadDigest")
	response, err = getResponse(t, client, badURL)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, response.StatusCode)
	_ = readResponse(t, response)
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

func TestS3ListCopyLogicalDeleteAndWorker(t *testing.T) {
	environment := newIntegrationEnvironment(t)
	client := environment.server.Client()
	sourceURL := environment.server.URL + "/hackmd/reports/2026/source.txt"
	copyURL := environment.server.URL + "/hackmd/reports/2026/copy.txt"
	content := []byte("copy without Telegram upload")

	request := authenticatedRequest(t, http.MethodPut, sourceURL, bytes.NewReader(content))
	request.Header.Set("Content-Type", "text/custom")
	request.Header.Set("X-Amz-Meta-Team", "storage")
	response, err := client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	sourceETag := response.Header.Get("ETag")
	require.NotEmpty(t, sourceETag)
	_ = readResponse(t, response)

	listRequest := authenticatedRequest(
		t,
		http.MethodGet,
		environment.server.URL+"/hackmd?list-type=2&prefix=reports/&delimiter=/",
		nil,
	)
	response, err = client.Do(listRequest)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Contains(t, string(readResponse(t, response)), "<Prefix>reports/2026/</Prefix>")

	copyRequest := authenticatedRequest(t, http.MethodPut, copyURL, nil)
	copyRequest.Header.Set("X-Amz-Copy-Source", "/hackmd/reports/2026/source.txt")
	response, err = client.Do(copyRequest)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Contains(t, string(readResponse(t, response)), strings.Trim(sourceETag, `"`))

	response, err = getResponse(t, client, copyURL)
	require.NoError(t, err)
	require.Equal(t, "text/custom", response.Header.Get("Content-Type"))
	require.Equal(t, "storage", response.Header.Get("X-Amz-Meta-Team"))
	require.Equal(t, content, readResponse(t, response))

	require.Equal(t, 1, queryIntegrationCount(
		t,
		environment.database,
		"SELECT COUNT(DISTINCT file_id) FROM tg_file_part_tab",
	))
	require.Equal(t, 2, queryIntegrationCount(
		t,
		environment.database,
		"SELECT COUNT(*) FROM tg_s3_object_metadata_tab",
	))

	deleteRequest := authenticatedRequest(t, http.MethodDelete, sourceURL, nil)
	response, err = client.Do(deleteRequest)
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, response.StatusCode)
	_ = readResponse(t, response)
	require.Zero(t, queryIntegrationCount(
		t,
		environment.database,
		"SELECT COUNT(*) FROM tg_file_part_delete_state_tab WHERE delete_state = 'pending'",
	))

	deleteRequest = authenticatedRequest(t, http.MethodDelete, copyURL, nil)
	response, err = client.Do(deleteRequest)
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, response.StatusCode)
	_ = readResponse(t, response)
	partCount := queryIntegrationCount(
		t,
		environment.database,
		"SELECT COUNT(*) FROM tg_file_part_tab",
	)
	require.Positive(t, partCount)
	require.Equal(t, partCount, queryIntegrationCount(
		t,
		environment.database,
		"SELECT COUNT(*) FROM tg_file_part_delete_state_tab WHERE delete_state = 'pending'",
	))

	workerContext, cancel := context.WithCancel(t.Context())
	workerDone := make(chan error, 1)
	go func() {
		workerDone <- environment.manager.RunBlockDeleteWorker(workerContext)
	}()
	require.Eventually(t, func() bool {
		return queryIntegrationCount(
			t,
			environment.database,
			"SELECT COUNT(*) FROM tg_file_part_delete_state_tab WHERE delete_state = 'deleted'",
		) == partCount
	}, 3*time.Second, 20*time.Millisecond)
	cancel()
	require.NoError(t, <-workerDone)
	require.Equal(t, partCount, queryIntegrationCount(
		t,
		environment.database,
		"SELECT COUNT(*) FROM tg_file_part_tab",
	))
}

func TestS3ListObjectsV1Compatibility(t *testing.T) {
	environment := newIntegrationEnvironment(t)
	client := environment.server.Client()
	objects := map[string]string{
		"alpha.txt":              "alpha",
		"reports/space name.txt": "space",
		"reports/sub/item.txt":   "nested",
		"z-last.txt":             "last",
	}
	for key, content := range objects {
		escapedKey := strings.ReplaceAll(url.PathEscape(key), "%2F", "/")
		request := authenticatedRequest(
			t,
			http.MethodPut,
			environment.server.URL+"/hackmd/"+escapedKey,
			strings.NewReader(content),
		)
		response, err := client.Do(request)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, response.StatusCode)
		_ = readResponse(t, response)
	}

	response, err := getResponse(t, client, environment.server.URL+"/hackmd/?delimiter=/")
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	require.Contains(t, string(readResponse(t, response)), "AccessDenied")

	request := authenticatedRequest(
		t,
		http.MethodGet,
		environment.server.URL+"/hackmd/?delimiter=/",
		nil,
	)
	response, err = client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	root := decodeListObjectsV1(t, readResponse(t, response))
	require.Equal(t, "hackmd", root.Name)
	require.Empty(t, root.Prefix)
	require.Empty(t, root.Marker)
	require.Equal(t, "/", root.Delimiter)
	require.Equal(t, 1000, root.MaxKeys)
	require.False(t, root.IsTruncated)
	require.Len(t, root.Contents, 2)
	require.Equal(t, []string{"alpha.txt", "z-last.txt"}, []string{
		root.Contents[0].Key,
		root.Contents[1].Key,
	})
	require.NotNil(t, root.Contents[0].Owner)
	require.Equal(t, "tgfile", root.Contents[0].Owner.ID)
	require.Equal(t, "tgfile", root.Contents[0].Owner.DisplayName)
	require.NotEmpty(t, root.Contents[0].LastModified)
	require.NotEmpty(t, root.Contents[0].ETag)
	require.Equal(t, int64(len(objects["alpha.txt"])), root.Contents[0].Size)
	require.Equal(t, "STANDARD", root.Contents[0].StorageClass)
	require.Len(t, root.CommonPrefixes, 1)
	require.Equal(t, []string{"reports/"}, []string{root.CommonPrefixes[0].Prefix})

	request = authenticatedRequest(t, http.MethodGet, environment.server.URL+"/hackmd/", nil)
	response, err = client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	trailingSlash := decodeListObjectsV1(t, readResponse(t, response))
	require.Len(t, trailingSlash.Contents, len(objects))
	require.Empty(t, trailingSlash.CommonPrefixes)

	request = authenticatedRequest(t, http.MethodGet, environment.server.URL+"/hackmd", nil)
	response, err = client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Contains(t, string(readResponse(t, response)), "<LocationConstraint")

	request = authenticatedRequest(
		t,
		http.MethodGet,
		environment.server.URL+"/hackmd?prefix=reports/&delimiter=/",
		nil,
	)
	response, err = client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	reports := decodeListObjectsV1(t, readResponse(t, response))
	require.Equal(t, "reports/", reports.Prefix)
	require.Len(t, reports.Contents, 1)
	require.Equal(t, []string{"reports/space name.txt"}, []string{reports.Contents[0].Key})
	require.Len(t, reports.CommonPrefixes, 1)
	require.Equal(t, []string{"reports/sub/"}, []string{reports.CommonPrefixes[0].Prefix})

	request = authenticatedRequest(
		t,
		http.MethodGet,
		environment.server.URL+"/hackmd/?prefix=reports/&encoding-type=url",
		nil,
	)
	response, err = client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	encoded := decodeListObjectsV1(t, readResponse(t, response))
	require.Equal(t, "url", encoded.EncodingType)
	require.Equal(t, "reports/", encoded.Prefix)
	require.Len(t, encoded.Contents, 2)
	require.Equal(t, []string{
		"reports/space%20name.txt",
		"reports/sub/item.txt",
	}, []string{
		encoded.Contents[0].Key,
		encoded.Contents[1].Key,
	})

	marker := ""
	expectedPages := []struct {
		key          string
		commonPrefix string
		nextMarker   string
		truncated    bool
	}{
		{key: "alpha.txt", nextMarker: "alpha.txt", truncated: true},
		{commonPrefix: "reports/", nextMarker: "reports/", truncated: true},
		{key: "z-last.txt"},
	}
	for _, expected := range expectedPages {
		query := url.Values{"delimiter": {"/"}, "max-keys": {"1"}}
		if marker != "" {
			query.Set("marker", marker)
		}
		request = authenticatedRequest(
			t,
			http.MethodGet,
			environment.server.URL+"/hackmd/?"+query.Encode(),
			nil,
		)
		response, err = client.Do(request)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, response.StatusCode)
		page := decodeListObjectsV1(t, readResponse(t, response))
		require.Equal(t, marker, page.Marker)
		require.Equal(t, expected.nextMarker, page.NextMarker)
		require.Equal(t, expected.truncated, page.IsTruncated)
		if expected.key == "" {
			require.Empty(t, page.Contents)
			require.Len(t, page.CommonPrefixes, 1)
			require.Equal(t, expected.commonPrefix, page.CommonPrefixes[0].Prefix)
		} else {
			require.Empty(t, page.CommonPrefixes)
			require.Len(t, page.Contents, 1)
			require.Equal(t, expected.key, page.Contents[0].Key)
		}
		marker = page.NextMarker
	}

	request = authenticatedRequest(
		t,
		http.MethodGet,
		environment.server.URL+"/hackmd/?prefix=missing/",
		nil,
	)
	response, err = client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	empty := decodeListObjectsV1(t, readResponse(t, response))
	require.Empty(t, empty.Contents)
	require.Empty(t, empty.CommonPrefixes)
	require.False(t, empty.IsTruncated)

	request = authenticatedRequest(
		t,
		http.MethodGet,
		environment.server.URL+"/hackmd/?max-keys=0",
		nil,
	)
	response, err = client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	zeroMaxKeys := decodeListObjectsV1(t, readResponse(t, response))
	require.Zero(t, zeroMaxKeys.MaxKeys)
	require.Empty(t, zeroMaxKeys.Contents)
	require.Empty(t, zeroMaxKeys.CommonPrefixes)
	require.False(t, zeroMaxKeys.IsTruncated)

	for _, target := range []string{
		environment.server.URL + "/hackmd/?prefix=a&prefix=b",
		environment.server.URL + "/hackmd/?delimiter=:",
		environment.server.URL + "/hackmd/?max-keys=1001",
	} {
		request = authenticatedRequest(t, http.MethodGet, target, nil)
		response, err = client.Do(request)
		require.NoError(t, err)
		require.Equal(t, http.StatusBadRequest, response.StatusCode)
		require.Contains(t, string(readResponse(t, response)), "InvalidArgument")
	}

	request = authenticatedRequest(
		t,
		http.MethodGet,
		environment.server.URL+"/hackmd/?list-type=1",
		nil,
	)
	response, err = client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotImplemented, response.StatusCode)
	require.Contains(t, string(readResponse(t, response)), "NotImplemented")

	request = authenticatedRequest(
		t,
		http.MethodGet,
		environment.server.URL+"/unknown/?delimiter=/",
		nil,
	)
	response, err = client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, response.StatusCode)
	require.Contains(t, string(readResponse(t, response)), "NoSuchBucket")

	request = authenticatedRequest(t, http.MethodHead, environment.server.URL+"/hackmd/", nil)
	response, err = client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Empty(t, readResponse(t, response))
}

func TestS3ACLUnknownBucketAndDeleteObjects(t *testing.T) {
	environment := newIntegrationEnvironment(t)
	client := environment.server.Client()
	privateURL := environment.server.URL + "/private-data/object.txt"

	response, err := getResponse(t, client, privateURL)
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	_ = readResponse(t, response)

	response, err = getResponse(t, client, environment.server.URL+"/unknown/object")
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	_ = readResponse(t, response)

	request := authenticatedRequest(t, http.MethodGet, environment.server.URL+"/unknown/object", nil)
	response, err = client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, response.StatusCode)
	require.Contains(t, string(readResponse(t, response)), "NoSuchBucket")

	for _, key := range []string{"one", "two"} {
		request = authenticatedRequest(
			t,
			http.MethodPut,
			environment.server.URL+"/private-data/"+key,
			bytes.NewReader(nil),
		)
		response, err = client.Do(request)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, response.StatusCode)
		_ = readResponse(t, response)
	}
	deleteXML := `<Delete xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Object><Key>one</Key></Object>` +
		`<Object><Key>two</Key></Object></Delete>`
	request = authenticatedRequest(
		t,
		http.MethodPost,
		environment.server.URL+"/private-data?delete",
		strings.NewReader(deleteXML),
	)
	request.Header.Set("Content-MD5", "ZS9nCCmfXNH5p8J+JxeJvg==")
	response, err = client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	result := string(readResponse(t, response))
	require.Contains(t, result, `xmlns="http://s3.amazonaws.com/doc/2006-03-01/"`)
	require.Contains(t, result, "<Key>one</Key>")
	require.Contains(t, result, "<Key>two</Key>")

	request = authenticatedRequest(
		t,
		http.MethodPut,
		environment.server.URL+"/private-data/three",
		bytes.NewReader(nil),
	)
	response, err = client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	_ = readResponse(t, response)

	deleteXML = `<Delete xmlns="http://s3.amazonaws.com/doc/2006-03-01/">` +
		`<Object><Key>three</Key></Object></Delete>`
	request = authenticatedRequest(
		t,
		http.MethodPost,
		environment.server.URL+"/private-data/?delete",
		strings.NewReader(deleteXML),
	)
	request.Header.Set("Content-MD5", "9ViRvicg+u/y5r5gpMdVqA==")
	response, err = client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Contains(t, string(readResponse(t, response)), "<Key>three</Key>")
}

func TestS3HistoricalKeyCompatibilityCannotCrossBucketBoundary(t *testing.T) {
	environment := newIntegrationEnvironment(t)
	client := environment.server.Client()
	privateURL := environment.server.URL + "/private-data/secret.txt"
	request := authenticatedRequest(
		t,
		http.MethodPut,
		privateURL,
		bytes.NewReader([]byte("private-content")),
	)
	response, err := client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	_ = readResponse(t, response)

	response, err = getResponse(
		t,
		client,
		environment.server.URL+"/hackmd/../private-data/secret.txt",
	)
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, response.StatusCode)
	require.NotContains(t, string(readResponse(t, response)), "private-content")

	request = authenticatedRequest(
		t,
		http.MethodPut,
		environment.server.URL+"/hackmd/copied.txt",
		nil,
	)
	request.Header.Set("X-Amz-Copy-Source", "/hackmd/../private-data/secret.txt")
	response, err = client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, response.StatusCode)
	_ = readResponse(t, response)

	response, err = getResponse(t, client, privateURL)
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	_ = readResponse(t, response)
}

func TestS3UnsupportedObjectSubresourcesAreNotDispatchedAsObjectIO(t *testing.T) {
	environment := newIntegrationEnvironment(t)
	client := environment.server.Client()
	objectURL := environment.server.URL + "/hackmd/object.txt"
	request := authenticatedRequest(
		t,
		http.MethodPut,
		objectURL,
		bytes.NewReader([]byte("content")),
	)
	response, err := client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	_ = readResponse(t, response)

	for _, target := range []string{
		objectURL + "?acl",
		objectURL + "?policy",
		objectURL + "?cors",
		objectURL + "?versioning",
		environment.server.URL + "/hackmd/?acl",
		environment.server.URL + "/hackmd/?policy",
		environment.server.URL + "/hackmd/?cors",
		environment.server.URL + "/hackmd/?versioning",
	} {
		request = authenticatedRequest(t, http.MethodGet, target, nil)
		response, err = client.Do(request)
		require.NoError(t, err)
		require.Equal(t, http.StatusNotImplemented, response.StatusCode)
		require.Contains(t, string(readResponse(t, response)), "NotImplemented")
	}

	request = authenticatedRequest(t, http.MethodPost, objectURL+"?uploads", nil)
	response, err = client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Contains(t, string(readResponse(t, response)), "InitiateMultipartUploadResult")

	response, err = getResponse(t, client, objectURL)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, []byte("content"), readResponse(t, response))

	request = authenticatedRequest(t, http.MethodGet, objectURL+"?cache-bust=1", nil)
	response, err = client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, []byte("content"), readResponse(t, response))
}

func TestS3MultipartUploadIsReadableThroughS3AndWebDAV(t *testing.T) {
	environment := newIntegrationEnvironment(t)
	client := environment.server.Client()
	objectURL := environment.server.URL + "/hackmd/multipart/data.bin"

	anonymousCreate, err := http.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		objectURL+"?uploads",
		nil,
	)
	require.NoError(t, err)
	response, err := client.Do(anonymousCreate)
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	_ = readResponse(t, response)

	createRequest := authenticatedRequest(t, http.MethodPost, objectURL+"?uploads", nil)
	createRequest.Header.Set("Content-Type", "application/custom")
	createRequest.Header.Set("X-Amz-Meta-Source", "multipart")
	response, err = client.Do(createRequest)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, "CRC64NVME", response.Header.Get("X-Amz-Checksum-Algorithm"))
	require.Equal(t, "FULL_OBJECT", response.Header.Get("X-Amz-Checksum-Type"))
	var initiated struct {
		UploadID string `xml:"UploadId"`
	}
	require.NoError(t, xml.Unmarshal(readResponse(t, response), &initiated))
	require.Len(t, initiated.UploadID, 64)

	firstContent := bytes.Repeat([]byte("a"), 5*1024*1024)
	secondContent := []byte("multipart-tail")
	firstETag := uploadIntegrationPart(t, client, objectURL, initiated.UploadID, 1, firstContent)
	secondETag := uploadIntegrationPart(t, client, objectURL, initiated.UploadID, 2, secondContent)

	listRequest := authenticatedRequest(
		t,
		http.MethodGet,
		objectURL+"?uploadId="+initiated.UploadID+"&max-parts=1",
		nil,
	)
	response, err = client.Do(listRequest)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	listBody := string(readResponse(t, response))
	require.Contains(t, listBody, "<IsTruncated>true</IsTruncated>")
	require.Contains(t, listBody, "<NextPartNumberMarker>1</NextPartNumberMarker>")
	require.Contains(t, listBody, "<ChecksumAlgorithm>CRC64NVME</ChecksumAlgorithm>")
	require.Contains(t, listBody, "<ChecksumType>FULL_OBJECT</ChecksumType>")
	require.Contains(t, listBody, "<ChecksumCRC64NVME>")

	completeBody := []byte(
		`<CompleteMultipartUpload xmlns="http://s3.amazonaws.com/doc/2006-03-01/">` +
			`<Part><PartNumber>1</PartNumber><ETag>` + firstETag + `</ETag></Part>` +
			`<Part><PartNumber>2</PartNumber><ETag>` + secondETag + `</ETag></Part>` +
			`</CompleteMultipartUpload>`,
	)
	completeRequest := authenticatedRequest(
		t,
		http.MethodPost,
		objectURL+"?uploadId="+initiated.UploadID,
		bytes.NewReader(completeBody),
	)
	completeDigest := filemgr.NewMD5CompatibilityHash()
	_, err = completeDigest.Write(completeBody)
	require.NoError(t, err)
	completeRequest.Header.Set("Content-MD5", base64.StdEncoding.EncodeToString(completeDigest.Sum(nil)))
	response, err = client.Do(completeRequest)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	finalETag := response.Header.Get("ETag")
	require.Contains(t, finalETag, "-2")
	finalHash, err := s3checksum.NewHash(s3checksum.AlgorithmCRC64NVME)
	require.NoError(t, err)
	allContent := append(bytes.Clone(firstContent), secondContent...)
	_, err = finalHash.Write(allContent)
	require.NoError(t, err)
	finalChecksum := s3checksum.SumBase64(finalHash)
	completeResponseBody := string(readResponse(t, response))
	require.Contains(t, completeResponseBody, "<CompleteMultipartUploadResult")
	require.Contains(t, completeResponseBody, "<ChecksumCRC64NVME>"+finalChecksum+"</ChecksumCRC64NVME>")
	require.Contains(t, completeResponseBody, "<ChecksumType>FULL_OBJECT</ChecksumType>")

	response, err = getResponse(t, client, objectURL)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, "application/custom", response.Header.Get("Content-Type"))
	require.Equal(t, "multipart", response.Header.Get("X-Amz-Meta-Source"))
	require.Equal(t, finalChecksum, response.Header.Get("X-Amz-Checksum-Crc64nvme"))
	require.Equal(t, "FULL_OBJECT", response.Header.Get("X-Amz-Checksum-Type"))
	require.Equal(t, allContent, readResponse(t, response))

	headRequest := authenticatedRequest(t, http.MethodHead, objectURL, nil)
	headRequest.Header.Set("X-Amz-Checksum-Mode", "ENABLED")
	response, err = client.Do(headRequest)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, finalChecksum, response.Header.Get("X-Amz-Checksum-Crc64nvme"))
	require.Equal(t, "FULL_OBJECT", response.Header.Get("X-Amz-Checksum-Type"))
	_ = readResponse(t, response)

	webDAVRequest := authenticatedRequest(
		t,
		http.MethodGet,
		environment.server.URL+"/webdav/hackmd/multipart/data.bin",
		nil,
	)
	response, err = client.Do(webDAVRequest)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, append(firstContent, secondContent...), readResponse(t, response))

	webDAVHead := authenticatedRequest(
		t,
		http.MethodHead,
		environment.server.URL+"/webdav/hackmd/multipart/data.bin",
		nil,
	)
	response, err = client.Do(webDAVHead)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, strconv.Itoa(len(firstContent)+len(secondContent)), response.Header.Get("Content-Length"))
	_ = readResponse(t, response)

	webDAVRange := authenticatedRequest(
		t,
		http.MethodGet,
		environment.server.URL+"/webdav/hackmd/multipart/data.bin",
		nil,
	)
	webDAVRange.Header.Set("Range", "bytes=5242878-5242884")
	response, err = client.Do(webDAVRange)
	require.NoError(t, err)
	require.Equal(t, http.StatusPartialContent, response.StatusCode)
	require.Equal(t, append(firstContent, secondContent...)[5242878:5242885], readResponse(t, response))

	propfind := authenticatedRequest(
		t,
		"PROPFIND",
		environment.server.URL+"/webdav/hackmd/multipart",
		nil,
	)
	propfind.Header.Set("Depth", "1")
	response, err = client.Do(propfind)
	require.NoError(t, err)
	require.Equal(t, http.StatusMultiStatus, response.StatusCode)
	require.Contains(t, string(readResponse(t, response)), "data.bin")

	webDAVCopyURL := environment.server.URL + "/webdav/hackmd/multipart/copy.bin"
	webDAVCopy := authenticatedRequest(
		t,
		"COPY",
		environment.server.URL+"/webdav/hackmd/multipart/data.bin",
		nil,
	)
	webDAVCopy.Header.Set("Destination", webDAVCopyURL)
	webDAVCopy.Header.Set("Overwrite", "F")
	response, err = client.Do(webDAVCopy)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, response.StatusCode)
	_ = readResponse(t, response)

	webDAVDelete := authenticatedRequest(
		t,
		http.MethodDelete,
		environment.server.URL+"/webdav/hackmd/multipart/data.bin",
		nil,
	)
	response, err = client.Do(webDAVDelete)
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, response.StatusCode)
	_ = readResponse(t, response)
	require.Zero(t, queryIntegrationCount(
		t,
		environment.database,
		"SELECT COUNT(*) FROM tg_file_part_delete_state_tab WHERE delete_state = 'pending'",
	))
	response, err = client.Do(authenticatedRequest(t, http.MethodGet, webDAVCopyURL, nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, append(firstContent, secondContent...), readResponse(t, response))

	webDAVMovedURL := environment.server.URL + "/webdav/hackmd/multipart/moved.bin"
	webDAVMove := authenticatedRequest(t, "MOVE", webDAVCopyURL, nil)
	webDAVMove.Header.Set("Destination", webDAVMovedURL)
	webDAVMove.Header.Set("Overwrite", "F")
	response, err = client.Do(webDAVMove)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, response.StatusCode)
	_ = readResponse(t, response)
	response, err = client.Do(authenticatedRequest(t, http.MethodGet, webDAVCopyURL, nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, response.StatusCode)
	_ = readResponse(t, response)

	webDAVDelete = authenticatedRequest(t, http.MethodDelete, webDAVMovedURL, nil)
	response, err = client.Do(webDAVDelete)
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, response.StatusCode)
	_ = readResponse(t, response)
	partCount := queryIntegrationCount(
		t,
		environment.database,
		"SELECT COUNT(*) FROM tg_file_part_tab",
	)
	require.Positive(t, partCount)
	require.Equal(t, partCount, queryIntegrationCount(
		t,
		environment.database,
		"SELECT COUNT(*) FROM tg_file_part_delete_state_tab WHERE delete_state = 'pending'",
	))
	response, err = getResponse(t, client, objectURL)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, response.StatusCode)
	_ = readResponse(t, response)

	workerContext, cancelWorker := context.WithCancel(t.Context())
	workerDone := make(chan error, 1)
	go func() {
		workerDone <- environment.manager.RunBlockDeleteWorker(workerContext)
	}()
	require.Eventually(t, func() bool {
		return queryIntegrationCount(
			t,
			environment.database,
			"SELECT COUNT(*) FROM tg_file_part_delete_state_tab WHERE delete_state = 'deleted'",
		) == partCount
	}, 3*time.Second, 20*time.Millisecond)
	cancelWorker()
	require.NoError(t, <-workerDone)

	abortRequest := authenticatedRequest(
		t,
		http.MethodDelete,
		objectURL+"?uploadId="+initiated.UploadID,
		nil,
	)
	response, err = client.Do(abortRequest)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, response.StatusCode)
	require.Contains(t, string(readResponse(t, response)), "NoSuchUpload")
}

func TestS3MultipartChecksumAlgorithmsInteroperate(t *testing.T) {
	environment := newIntegrationEnvironment(t)
	client := environment.server.Client()
	content := []byte("multipart checksum integration payload")
	algorithms := []s3checksum.Algorithm{
		s3checksum.AlgorithmCRC32,
		s3checksum.AlgorithmCRC32C,
		s3checksum.AlgorithmCRC64NVME,
		s3checksum.AlgorithmSHA1,
		s3checksum.AlgorithmSHA256,
	}
	for _, algorithm := range algorithms {
		t.Run(string(algorithm), func(t *testing.T) {
			checksumType := s3checksum.TypeComposite
			if algorithm == s3checksum.AlgorithmCRC64NVME {
				checksumType = s3checksum.TypeFullObject
			}
			key := "checksum/algorithm-" + strings.ToLower(string(algorithm)) + ".bin"
			objectURL := environment.server.URL + "/hackmd/" + key
			uploadID := createChecksumIntegrationMultipart(
				t,
				client,
				objectURL,
				algorithm,
				checksumType,
			)
			partChecksum := integrationChecksumValue(t, algorithm, content)
			partETag := uploadChecksumIntegrationPart(
				t,
				client,
				objectURL,
				uploadID,
				algorithm,
				content,
				partChecksum,
			)
			assertChecksumIntegrationListParts(
				t,
				client,
				objectURL,
				uploadID,
				algorithm,
				checksumType,
				partChecksum,
			)
			finalRequestValue := partChecksum
			finalResponseValue := partChecksum
			if checksumType == s3checksum.TypeComposite {
				var err error
				finalRequestValue, finalResponseValue, err = s3checksum.Composite(
					algorithm,
					[]string{partChecksum},
				)
				require.NoError(t, err)
			}
			completeChecksumIntegrationMultipart(
				t,
				client,
				objectURL,
				uploadID,
				algorithm,
				checksumType,
				partETag,
				partChecksum,
				finalRequestValue,
				finalResponseValue,
				int64(len(content)),
			)
			assertChecksumIntegrationObject(
				t,
				client,
				objectURL,
				algorithm,
				checksumType,
				finalResponseValue,
				content,
			)
		})
	}
}

func createChecksumIntegrationMultipart(
	t *testing.T,
	client *http.Client,
	objectURL string,
	algorithm s3checksum.Algorithm,
	checksumType s3checksum.Type,
) string {
	t.Helper()
	request := authenticatedRequest(t, http.MethodPost, objectURL+"?uploads", nil)
	request.Header.Set("X-Amz-Checksum-Algorithm", string(algorithm))
	request.Header.Set("X-Amz-Checksum-Type", string(checksumType))
	response, err := client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, string(algorithm), response.Header.Get("X-Amz-Checksum-Algorithm"))
	require.Equal(t, string(checksumType), response.Header.Get("X-Amz-Checksum-Type"))
	var result struct {
		UploadID string `xml:"UploadId"`
	}
	require.NoError(t, xml.Unmarshal(readResponse(t, response), &result))
	require.Len(t, result.UploadID, 64)
	return result.UploadID
}

func uploadChecksumIntegrationPart(
	t *testing.T,
	client *http.Client,
	objectURL, uploadID string,
	algorithm s3checksum.Algorithm,
	content []byte,
	checksumValue string,
) string {
	t.Helper()
	target := objectURL + "?partNumber=1&uploadId=" + uploadID
	request := authenticatedRequest(t, http.MethodPut, target, bytes.NewReader(content))
	header, err := s3checksum.HeaderName(algorithm)
	require.NoError(t, err)
	request.Header.Set(header, checksumValue)
	request.Header.Set("X-Amz-Sdk-Checksum-Algorithm", string(algorithm))
	response, err := client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, checksumValue, response.Header.Get(header))
	_ = readResponse(t, response)
	etag := response.Header.Get("ETag")
	require.Len(t, etag, 34)
	return etag
}

func assertChecksumIntegrationListParts(
	t *testing.T,
	client *http.Client,
	objectURL, uploadID string,
	algorithm s3checksum.Algorithm,
	checksumType s3checksum.Type,
	checksumValue string,
) {
	t.Helper()
	request := authenticatedRequest(t, http.MethodGet, objectURL+"?uploadId="+uploadID, nil)
	response, err := client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	body := string(readResponse(t, response))
	require.Contains(t, body, "<ChecksumAlgorithm>"+string(algorithm)+"</ChecksumAlgorithm>")
	require.Contains(t, body, "<ChecksumType>"+string(checksumType)+"</ChecksumType>")
	require.Contains(t, body, "<Checksum"+string(algorithm)+">"+checksumValue+"</Checksum"+string(algorithm)+">")
}

func completeChecksumIntegrationMultipart(
	t *testing.T,
	client *http.Client,
	objectURL, uploadID string,
	algorithm s3checksum.Algorithm,
	checksumType s3checksum.Type,
	etag, partChecksum, finalRequestValue, finalResponseValue string,
	objectSize int64,
) {
	t.Helper()
	body := []byte(fmt.Sprintf(
		"<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>%s</ETag>"+
			"<Checksum%s>%s</Checksum%s></Part></CompleteMultipartUpload>",
		etag,
		algorithm,
		partChecksum,
		algorithm,
	))
	request := authenticatedRequest(
		t,
		http.MethodPost,
		objectURL+"?uploadId="+uploadID,
		bytes.NewReader(body),
	)
	header, err := s3checksum.HeaderName(algorithm)
	require.NoError(t, err)
	request.Header.Set(header, finalRequestValue)
	request.Header.Set("X-Amz-Checksum-Type", string(checksumType))
	request.Header.Set("X-Amz-Mp-Object-Size", strconv.FormatInt(objectSize, 10))
	response, err := client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	responseBody := string(readResponse(t, response))
	require.Contains(
		t,
		responseBody,
		"<Checksum"+string(algorithm)+">"+finalResponseValue+"</Checksum"+string(algorithm)+">",
	)
	require.Contains(t, responseBody, "<ChecksumType>"+string(checksumType)+"</ChecksumType>")
}

func assertChecksumIntegrationObject(
	t *testing.T,
	client *http.Client,
	objectURL string,
	algorithm s3checksum.Algorithm,
	checksumType s3checksum.Type,
	checksumValue string,
	content []byte,
) {
	t.Helper()
	header, err := s3checksum.HeaderName(algorithm)
	require.NoError(t, err)
	head := authenticatedRequest(t, http.MethodHead, objectURL, nil)
	head.Header.Set("X-Amz-Checksum-Mode", "ENABLED")
	response, err := client.Do(head)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, checksumValue, response.Header.Get(header))
	require.Equal(t, string(checksumType), response.Header.Get("X-Amz-Checksum-Type"))
	_ = readResponse(t, response)

	response, err = getResponse(t, client, objectURL)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, content, readResponse(t, response))

	serverURL, _, found := strings.Cut(objectURL, "/hackmd/")
	require.True(t, found)
	list := authenticatedRequest(
		t,
		http.MethodGet,
		serverURL+"/hackmd?list-type=2&prefix=checksum%2F",
		nil,
	)
	response, err = client.Do(list)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	listBody := string(readResponse(t, response))
	require.Contains(t, listBody, "<ChecksumAlgorithm>"+string(algorithm)+"</ChecksumAlgorithm>")
	require.Contains(t, listBody, "<ChecksumType>"+string(checksumType)+"</ChecksumType>")

	copiedURL := objectURL + ".copy"
	copyRequest := authenticatedRequest(t, http.MethodPut, copiedURL, nil)
	copyRequest.Header.Set("X-Amz-Copy-Source", strings.TrimPrefix(objectURL, serverURL))
	copyRequest.Header.Set("X-Amz-Checksum-Algorithm", string(algorithm))
	response, err = client.Do(copyRequest)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	copyBody := string(readResponse(t, response))
	require.Contains(
		t,
		copyBody,
		"<Checksum"+string(algorithm)+">"+checksumValue+"</Checksum"+string(algorithm)+">",
	)
	require.Contains(t, copyBody, "<ChecksumType>"+string(checksumType)+"</ChecksumType>")

	copiedHead := authenticatedRequest(t, http.MethodHead, copiedURL, nil)
	copiedHead.Header.Set("X-Amz-Checksum-Mode", "ENABLED")
	response, err = client.Do(copiedHead)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, checksumValue, response.Header.Get(header))
	require.Equal(t, string(checksumType), response.Header.Get("X-Amz-Checksum-Type"))
	_ = readResponse(t, response)
}

func integrationChecksumValue(
	t *testing.T,
	algorithm s3checksum.Algorithm,
	content []byte,
) string {
	t.Helper()
	hasher, err := s3checksum.NewHash(algorithm)
	require.NoError(t, err)
	_, err = hasher.Write(content)
	require.NoError(t, err)
	return s3checksum.SumBase64(hasher)
}

func uploadIntegrationPart(
	t *testing.T,
	client *http.Client,
	objectURL, uploadID string,
	partNumber int,
	content []byte,
) string {
	t.Helper()
	target := fmt.Sprintf("%s?partNumber=%d&uploadId=%s", objectURL, partNumber, uploadID)
	request := authenticatedRequest(t, http.MethodPut, target, bytes.NewReader(content))
	response, err := client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	_ = readResponse(t, response)
	etag := response.Header.Get("ETag")
	require.Len(t, etag, 34)
	require.NotEmpty(t, response.Header.Get("X-Amz-Checksum-Crc64nvme"))
	return etag
}

func TestS3MultipartListAbortAndProtocolErrors(t *testing.T) {
	environment := newIntegrationEnvironment(t)
	client := environment.server.Client()
	firstURL := environment.server.URL + "/hackmd/active.bin"
	secondURL := environment.server.URL + "/hackmd/nested/active.bin"
	firstUploadID := createIntegrationMultipart(t, client, firstURL)
	_ = createIntegrationMultipart(t, client, secondURL)

	listRequest := authenticatedRequest(
		t,
		http.MethodGet,
		environment.server.URL+"/hackmd?uploads&delimiter=/&max-keys=1000",
		nil,
	)
	response, err := client.Do(listRequest)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	listBody := string(readResponse(t, response))
	require.Contains(t, listBody, "<Key>active.bin</Key>")
	require.Contains(t, listBody, "<Prefix>nested/</Prefix>")

	copyPart := authenticatedRequest(
		t,
		http.MethodPut,
		firstURL+"?partNumber=1&uploadId="+firstUploadID,
		bytes.NewReader([]byte("copy")),
	)
	copyPart.Header.Set("X-Amz-Copy-Source", "/hackmd/source.bin")
	response, err = client.Do(copyPart)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotImplemented, response.StatusCode)
	require.Contains(t, string(readResponse(t, response)), "NotImplemented")

	checksumPart := authenticatedRequest(
		t,
		http.MethodPut,
		firstURL+"?partNumber=1&uploadId="+firstUploadID,
		bytes.NewReader([]byte("checksum")),
	)
	checksumPart.Header.Set("X-Amz-Checksum-Sha256", "invalid")
	response, err = client.Do(checksumPart)
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, response.StatusCode)
	require.Contains(t, string(readResponse(t, response)), "InvalidDigest")

	_ = uploadIntegrationPart(t, client, firstURL, firstUploadID, 1, []byte("abort-me"))
	for range 2 {
		abort := authenticatedRequest(
			t,
			http.MethodDelete,
			firstURL+"?uploadId="+firstUploadID,
			nil,
		)
		response, err = client.Do(abort)
		require.NoError(t, err)
		require.Equal(t, http.StatusNoContent, response.StatusCode)
		_ = readResponse(t, response)
	}
	listParts := authenticatedRequest(
		t,
		http.MethodGet,
		firstURL+"?uploadId="+firstUploadID,
		nil,
	)
	response, err = client.Do(listParts)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, response.StatusCode)
	require.Contains(t, string(readResponse(t, response)), "NoSuchUpload")

	duplicateQuery := authenticatedRequest(
		t,
		http.MethodGet,
		secondURL+"?uploadId=one&uploadId=two",
		nil,
	)
	response, err = client.Do(duplicateQuery)
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, response.StatusCode)
	require.Contains(t, string(readResponse(t, response)), "InvalidArgument")
}

func createIntegrationMultipart(t *testing.T, client *http.Client, objectURL string) string {
	t.Helper()
	request := authenticatedRequest(t, http.MethodPost, objectURL+"?uploads", nil)
	response, err := client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	var initiated struct {
		UploadID string `xml:"UploadId"`
	}
	require.NoError(t, xml.Unmarshal(readResponse(t, response), &initiated))
	require.Len(t, initiated.UploadID, 64)
	return initiated.UploadID
}

func TestS3AWSV2SignerAndPresignedURLInteroperate(t *testing.T) {
	environment := newIntegrationEnvironment(t)
	client := environment.server.Client()
	credentials := aws.Credentials{AccessKeyID: "access", SecretAccessKey: "secret"}
	signer := awsv4.NewSigner()
	content := []byte("signed payload")
	payloadHashBytes := sha256.Sum256(content)
	payloadHash := hex.EncodeToString(payloadHashBytes[:])
	objectURL := environment.server.URL + "/private-data/signed.txt"

	request, err := http.NewRequestWithContext(t.Context(), http.MethodPut, objectURL, bytes.NewReader(content))
	require.NoError(t, err)
	request.Header.Set("X-Amz-Content-Sha256", payloadHash)
	require.NoError(t, signer.SignHTTP(
		t.Context(),
		credentials,
		request,
		payloadHash,
		"s3",
		"us-east-1",
		time.Now(),
	))
	response, err := client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	_ = readResponse(t, response)

	request, err = http.NewRequestWithContext(t.Context(), http.MethodGet, objectURL, nil)
	require.NoError(t, err)
	query := request.URL.Query()
	query.Set("X-Amz-Expires", "300")
	request.URL.RawQuery = query.Encode()
	const unsignedPayload = "UNSIGNED-PAYLOAD"
	signedURL, signedHeaders, err := signer.PresignHTTP(
		t.Context(),
		credentials,
		request,
		unsignedPayload,
		"s3",
		"us-east-1",
		time.Now(),
	)
	require.NoError(t, err)
	request, err = http.NewRequestWithContext(t.Context(), http.MethodGet, signedURL, nil)
	require.NoError(t, err)
	request.Header = signedHeaders
	response, err = client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, content, readResponse(t, response))

	tampered := strings.Replace(signedURL, "signed.txt", "other.txt", 1)
	request, err = http.NewRequestWithContext(t.Context(), http.MethodGet, tampered, nil)
	require.NoError(t, err)
	request.Header = signedHeaders
	response, err = client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	require.Contains(t, string(readResponse(t, response)), "SignatureDoesNotMatch")

	presignedObjectURL := environment.server.URL + "/private-data/presigned.txt"
	request, err = http.NewRequestWithContext(
		t.Context(),
		http.MethodPut,
		presignedObjectURL,
		bytes.NewReader([]byte("presigned body")),
	)
	require.NoError(t, err)
	query = request.URL.Query()
	query.Set("X-Amz-Expires", "300")
	request.URL.RawQuery = query.Encode()
	signedURL, signedHeaders, err = signer.PresignHTTP(
		t.Context(),
		credentials,
		request,
		unsignedPayload,
		"s3",
		"us-east-1",
		time.Now(),
	)
	require.NoError(t, err)
	request, err = http.NewRequestWithContext(
		t.Context(),
		http.MethodPut,
		signedURL,
		bytes.NewReader([]byte("presigned body")),
	)
	require.NoError(t, err)
	request.Header = signedHeaders
	response, err = client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	_ = readResponse(t, response)

	request, err = http.NewRequestWithContext(t.Context(), http.MethodDelete, presignedObjectURL, nil)
	require.NoError(t, err)
	query = request.URL.Query()
	query.Set("X-Amz-Expires", "300")
	request.URL.RawQuery = query.Encode()
	signedURL, signedHeaders, err = signer.PresignHTTP(
		t.Context(),
		credentials,
		request,
		unsignedPayload,
		"s3",
		"us-east-1",
		time.Now(),
	)
	require.NoError(t, err)
	request, err = http.NewRequestWithContext(t.Context(), http.MethodDelete, signedURL, nil)
	require.NoError(t, err)
	request.Header = signedHeaders
	response, err = client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, response.StatusCode)
	_ = readResponse(t, response)
}

func queryIntegrationCount(
	t *testing.T,
	database database.IDatabase,
	query string,
) int {
	t.Helper()
	rows, err := database.QueryContext(t.Context(), query)
	require.NoError(t, err)
	defer rows.Close()
	require.True(t, rows.Next())
	var count int
	require.NoError(t, rows.Scan(&count))
	require.NoError(t, rows.Err())
	return count
}
