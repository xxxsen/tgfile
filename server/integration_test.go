package server_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
	require.Equal(t, []byte("cond"), readResponse(t, response))

	request, err = http.NewRequestWithContext(t.Context(), http.MethodGet, objectURL, nil)
	require.NoError(t, err)
	request.Header.Set("Range", "bytes=0-3")
	request.Header.Set("If-Range", `"different"`)
	response, err = client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
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

	request = authenticatedRequest(t, http.MethodGet, objectURL+"?acl", nil)
	response, err = client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotImplemented, response.StatusCode)
	require.Contains(t, string(readResponse(t, response)), "NotImplemented")

	request = authenticatedRequest(t, http.MethodPost, objectURL+"?uploads", nil)
	response, err = client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotImplemented, response.StatusCode)
	require.Contains(t, string(readResponse(t, response)), "NotImplemented")

	response, err = getResponse(t, client, objectURL)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, []byte("content"), readResponse(t, response))
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
