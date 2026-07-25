package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
		time.Second,
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
		time.Second,
	)
	require.NoError(t, err)
	block := created.(*tgBlockIO)

	_, err = block.cacheGetDownloadLink(context.Background(), "missing-key")
	require.Error(t, err)
	require.Equal(t, int32(1), getFileCalls.Load())
}

func TestUploadTransportErrorDoesNotExposeBotToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(request.URL.Path, "/getMe") {
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"ok": true,
				"result": map[string]any{
					"id": 99, "is_bot": true, "first_name": "bot", "username": "bot",
				},
			})
			return
		}
		http.NotFound(writer, request)
	}))
	created, err := newWithEndpoint(
		1,
		"secret-token",
		server.URL+"/bot%s/%s",
		server.Client(),
		time.Second,
	)
	require.NoError(t, err)
	server.Close()

	_, err = created.Upload(t.Context(), strings.NewReader("content"))

	require.Error(t, err)
	require.NotContains(t, err.Error(), "secret-token")
	var networkError interface{ Timeout() bool }
	require.ErrorAs(t, err, &networkError)
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

func TestUploadReturnsStrictDeleteReferenceAndDeletesMessage(t *testing.T) {
	var deleteCalls atomic.Int32
	var deletedIDs []int
	var mutex sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(request.URL.Path, "/getMe"):
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"ok": true,
				"result": map[string]any{
					"id":         99,
					"is_bot":     true,
					"first_name": "bot",
					"username":   "bot",
				},
			})
		case strings.HasSuffix(request.URL.Path, "/sendDocument"):
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"ok": true,
				"result": map[string]any{
					"message_id": 42,
					"date":       1_700_000_000,
					"chat": map[string]any{
						"id":   1,
						"type": "private",
					},
					"document": map[string]any{
						"file_id":        "download-file-id",
						"file_unique_id": "unique",
						"file_name":      "object",
						"file_size":      7,
					},
				},
			})
		case strings.HasSuffix(request.URL.Path, "/deleteMessages"):
			deleteCalls.Add(1)
			var payload struct {
				ChatID     int64 `json:"chat_id"`
				MessageIDs []int `json:"message_ids"`
			}
			require.NoError(t, json.NewDecoder(request.Body).Decode(&payload))
			require.Equal(t, int64(1), payload.ChatID)
			mutex.Lock()
			deletedIDs = append(deletedIDs, payload.MessageIDs...)
			mutex.Unlock()
			_ = json.NewEncoder(writer).Encode(map[string]any{"ok": true, "result": true})
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
		time.Second,
	)
	require.NoError(t, err)
	block := created.(*tgBlockIO)

	result, err := block.Upload(t.Context(), strings.NewReader("content"))
	require.NoError(t, err)
	require.Equal(t, "download-file-id", result.FileKey)
	require.Equal(t, int64(1_700_000_000_000), result.UploadedAt)
	require.NotContains(t, result.DeleteRef, "token")

	require.NoError(t, block.DeleteBlocks(t.Context(), []string{result.DeleteRef}))
	require.Equal(t, int32(1), deleteCalls.Load())
	mutex.Lock()
	require.Equal(t, []int{42}, deletedIDs)
	mutex.Unlock()

	deleteCalls.Store(0)
	require.Error(t, block.DeleteBlocks(t.Context(), []string{
		`{"v":1,"bot_id":100,"chat_id":1,"message_id":42}`,
	}))
	require.Zero(t, deleteCalls.Load())
	require.Error(t, block.DeleteBlocks(t.Context(), []string{
		`{"v":1,"bot_id":99,"chat_id":1,"message_id":42,"unknown":true}`,
	}))
	require.Zero(t, deleteCalls.Load())
}

func TestUploadRejectsMessageFromUnexpectedChat(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(request.URL.Path, "/getMe"):
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"ok": true,
				"result": map[string]any{
					"id": 99, "is_bot": true, "first_name": "bot", "username": "bot",
				},
			})
		case strings.HasSuffix(request.URL.Path, "/sendDocument"):
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"ok": true,
				"result": map[string]any{
					"message_id": 42,
					"date":       1_700_000_000,
					"chat":       map[string]any{"id": 2, "type": "private"},
					"document": map[string]any{
						"file_id": "file", "file_unique_id": "unique",
					},
				},
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
		time.Second,
	)
	require.NoError(t, err)

	_, err = created.Upload(t.Context(), strings.NewReader("content"))

	require.ErrorIs(t, err, errUploadDeleteRef)
}

func TestDeleteServerErrorWithoutJSONRemainsRetryable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusServiceUnavailable)
		_, _ = writer.Write([]byte("unavailable"))
	}))
	defer server.Close()
	block := &tgBlockIO{
		chatid:   1,
		token:    "token",
		endpoint: server.URL + "/bot%s/%s",
		client:   server.Client(),
		bot:      &tgbotapi.BotAPI{Self: tgbotapi.User{ID: 99}},
	}

	err := block.DeleteBlocks(t.Context(), []string{
		`{"v":1,"bot_id":99,"chat_id":1,"message_id":42}`,
	})

	var deleteError *DeleteError
	require.ErrorAs(t, err, &deleteError)
	require.Equal(t, http.StatusServiceUnavailable, deleteError.StatusCode)
}

func TestUploadsAreSerializedAndStartAtLeastOneSecondApart(t *testing.T) {
	var startsMu sync.Mutex
	starts := make([]time.Time, 0, 2)
	var messageID atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(request.URL.Path, "/getMe"):
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"ok": true,
				"result": map[string]any{
					"id": 99, "is_bot": true, "first_name": "bot", "username": "bot",
				},
			})
		case strings.HasSuffix(request.URL.Path, "/sendDocument"):
			startsMu.Lock()
			starts = append(starts, time.Now())
			startsMu.Unlock()
			id := messageID.Add(1)
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"ok": true,
				"result": map[string]any{
					"message_id": id,
					"date":       1_700_000_000,
					"chat":       map[string]any{"id": 1, "type": "private"},
					"document": map[string]any{
						"file_id": fmt.Sprintf("file-%d", id), "file_unique_id": fmt.Sprintf("unique-%d", id),
					},
				},
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
		time.Second,
	)
	require.NoError(t, err)
	block := created.(*tgBlockIO)

	var waitGroup sync.WaitGroup
	waitGroup.Add(2)
	errorsChannel := make(chan error, 2)
	for range 2 {
		go func() {
			defer waitGroup.Done()
			_, uploadErr := block.Upload(t.Context(), strings.NewReader("x"))
			errorsChannel <- uploadErr
		}()
	}
	waitGroup.Wait()
	close(errorsChannel)
	for uploadErr := range errorsChannel {
		require.NoError(t, uploadErr)
	}
	startsMu.Lock()
	require.Len(t, starts, 2)
	require.GreaterOrEqual(t, starts[1].Sub(starts[0]), time.Second-20*time.Millisecond)
	startsMu.Unlock()
}
