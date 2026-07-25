package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	lru "github.com/hashicorp/golang-lru/v2/expirable"
	"github.com/stretchr/testify/require"
)

func TestDefaultHTTPClientTimeouts(t *testing.T) {
	require.Equal(t, 30*time.Minute, defaultHTTPClient.Timeout)
	transport, ok := defaultHTTPClient.Transport.(*http.Transport)
	require.True(t, ok)
	require.Equal(t, 10*time.Second, transport.TLSHandshakeTimeout)
	require.Equal(t, 120*time.Second, transport.ResponseHeaderTimeout)
	require.Equal(t, 120*time.Second, transport.IdleConnTimeout)
	require.NotNil(t, transport.DialContext)
}

func TestContextReaderStopsAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reader := &contextReader{ctx: ctx, reader: strings.NewReader("content")}
	cancel()

	_, err := reader.Read(make([]byte, 8))
	require.ErrorIs(t, err, context.Canceled)
}

func TestDownloadRetriesReadOnlyServerErrors(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		call := calls.Add(1)
		if call < 3 {
			writer.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		require.Equal(t, "bytes=2-", request.Header.Get("Range"))
		writer.Header().Set("Content-Range", "bytes 2-4/5")
		writer.WriteHeader(http.StatusPartialContent)
		_, _ = writer.Write([]byte("cde"))
	}))
	defer server.Close()

	cache := lru.NewLRU[string, string](10, nil, time.Minute)
	_ = cache.Add("file-key", server.URL)
	block := &tgBlockIO{
		client:    server.Client(),
		linkCache: cache,
	}

	reader, err := block.Download(context.Background(), "file-key", 2)
	require.NoError(t, err)
	defer reader.Close()
	raw, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.Equal(t, []byte("cde"), raw)
	require.Equal(t, int32(3), calls.Load())
}

func TestGetFileRetriesButUploadDoesNot(t *testing.T) {
	var getFileCalls atomic.Int32
	var uploadCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(request.URL.Path, "/getMe"):
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"ok": true,
				"result": map[string]any{
					"id":         1,
					"is_bot":     true,
					"first_name": "bot",
					"username":   "bot",
				},
			})
		case strings.HasSuffix(request.URL.Path, "/getFile"):
			call := getFileCalls.Add(1)
			if call < 3 {
				_ = json.NewEncoder(writer).Encode(map[string]any{
					"ok":          false,
					"error_code":  http.StatusInternalServerError,
					"description": "retry",
				})
				return
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"ok": true,
				"result": map[string]any{
					"file_id":        "file-key",
					"file_unique_id": "unique",
					"file_size":      1,
					"file_path":      "file.txt",
				},
			})
		case strings.HasSuffix(request.URL.Path, "/sendDocument"):
			uploadCalls.Add(1)
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"ok":          false,
				"error_code":  http.StatusInternalServerError,
				"description": "do not retry upload",
			})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	created, err := newWithEndpoint(
		1,
		"token",
		server.URL+"/bot%s/%s",
		server.Client(),
	)
	require.NoError(t, err)
	block := created.(*tgBlockIO)

	link, err := block.cacheGetDownloadLink(context.Background(), "file-key")
	require.NoError(t, err)
	require.Equal(t, "https://api.telegram.org/file/bottoken/file.txt", link)
	require.Equal(t, int32(3), getFileCalls.Load())

	_, err = block.Upload(context.Background(), bytes.NewReader([]byte("content")))
	require.Error(t, err)
	var telegramError *tgbotapi.Error
	require.True(t, errors.As(err, &telegramError))
	require.Equal(t, int32(1), uploadCalls.Load())
}

func TestGetFileDoesNotRetryPermanentClientError(t *testing.T) {
	var getFileCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(request.URL.Path, "/getMe"):
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"ok": true,
				"result": map[string]any{
					"id":         1,
					"is_bot":     true,
					"first_name": "bot",
					"username":   "bot",
				},
			})
		case strings.HasSuffix(request.URL.Path, "/getFile"):
			getFileCalls.Add(1)
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"ok":          false,
				"error_code":  http.StatusNotFound,
				"description": "not found",
			})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	created, err := newWithEndpoint(
		1,
		"token",
		server.URL+"/bot%s/%s",
		server.Client(),
	)
	require.NoError(t, err)
	block := created.(*tgBlockIO)

	_, err = block.cacheGetDownloadLink(context.Background(), "missing-key")
	require.Error(t, err)
	require.Equal(t, int32(1), getFileCalls.Load())
}

func TestDownloadDoesNotRetryPermanentClientError(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writer.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	cache := lru.NewLRU[string, string](10, nil, time.Minute)
	_ = cache.Add("file-key", server.URL)
	block := &tgBlockIO{
		client:    server.Client(),
		linkCache: cache,
	}

	_, err := block.Download(context.Background(), "file-key", 0)
	require.Error(t, err)
	require.Equal(t, int32(1), calls.Load())
}
