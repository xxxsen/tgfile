package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/xxxsen/common/utils"

	"github.com/xxxsen/tgfile/blockio"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/google/uuid"
	lru "github.com/hashicorp/golang-lru/v2/expirable"
)

const (
	defaultMaxFileSize         = 20 * 1024 * 1024
	defaultMaxFileLinkToCache  = 2000
	defaultMaxFileLinkCacheTTL = 30 * time.Minute
	defaultRetryCount          = 3
	defaultRetryDelay          = 100 * time.Millisecond
	defaultUploadMinInterval   = time.Second
	maxDeleteReferenceSize     = 1024
	maxDeleteBatchSize         = 100
)

var (
	ErrRangeUnsupported     = errors.New("download response does not support range")
	ErrUnexpectedHTTPStatus = errors.New("unexpected HTTP status")
	errUploadInterval       = errors.New("upload interval is below the minimum")
	errUploadDeleteRef      = errors.New("upload response lacks a deletable document reference")
	errDeleteBatchTooLarge  = errors.New("delete batch exceeds the Telegram limit")
	errDeleteRefSize        = errors.New("invalid Telegram delete reference size")
	errDeleteRefTrailing    = errors.New("invalid trailing Telegram delete reference data")
	errDeleteRefIdentity    = errors.New("invalid Telegram delete reference identity")

	defaultHTTPClient = &http.Client{
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 10 * time.Second,
			}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 120 * time.Second,
			IdleConnTimeout:       120 * time.Second,
			MaxIdleConns:          20,
		},
		Timeout: 30 * time.Minute,
	}
)

type tgBlockIO struct {
	chatid            int64
	token             string
	endpoint          string
	bot               *tgbotapi.BotAPI
	client            *http.Client
	linkCache         *lru.LRU[string, string]
	uploadMu          sync.Mutex
	lastUploadStart   time.Time
	uploadMinInterval time.Duration
	deleteMu          sync.Mutex
	lastDeleteStart   time.Time
}

func New(chatid int64, token string, uploadMinInterval time.Duration) (blockio.IBlockIO, error) {
	return newWithEndpoint(chatid, token, tgbotapi.APIEndpoint, defaultHTTPClient, uploadMinInterval)
}

func newWithEndpoint(
	chatid int64,
	token, endpoint string,
	client *http.Client,
	uploadMinInterval time.Duration,
) (blockio.IBlockIO, error) {
	if uploadMinInterval == 0 {
		uploadMinInterval = defaultUploadMinInterval
	}
	if uploadMinInterval < defaultUploadMinInterval {
		return nil, fmt.Errorf("%w: minimum %s", errUploadInterval, defaultUploadMinInterval)
	}
	cache := lru.NewLRU[string, string](defaultMaxFileLinkToCache, nil, defaultMaxFileLinkCacheTTL)
	bot, err := tgbotapi.NewBotAPIWithClient(token, endpoint, client)
	if err != nil {
		return nil, &operationError{operation: "initialization", cause: err}
	}
	return &tgBlockIO{
		chatid:            chatid,
		token:             token,
		endpoint:          endpoint,
		bot:               bot,
		client:            client,
		linkCache:         cache,
		uploadMinInterval: uploadMinInterval,
	}, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(buffer []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, fmt.Errorf("read upload body: %w", r.ctx.Err())
	default:
		read, err := r.reader.Read(buffer)
		if err != nil && err != io.EOF {
			return read, fmt.Errorf("read upload body: %w", err)
		}
		if err == io.EOF {
			return read, io.EOF
		}
		return read, nil
	}
}

func (t *tgBlockIO) MaxFileSize() int64 {
	return defaultMaxFileSize
}

func (t *tgBlockIO) Upload(ctx context.Context, r io.Reader) (*blockio.UploadResult, error) {
	t.uploadMu.Lock()
	defer t.uploadMu.Unlock()
	if err := t.waitForUploadSlot(ctx); err != nil {
		return nil, err
	}
	t.lastUploadStart = time.Now()
	sname := uuid.NewString()
	freader := tgbotapi.FileReader{
		Name:   sname,
		Reader: &contextReader{ctx: ctx, reader: r},
	}
	doc := tgbotapi.NewDocument(t.chatid, freader)
	doc.DisableNotification = true
	msg, err := t.bot.Send(doc)
	if err != nil {
		return nil, &operationError{operation: "upload", cause: err}
	}
	if msg.Document == nil || msg.Document.FileID == "" || t.bot.Self.ID == 0 ||
		msg.Chat.ID == 0 || msg.Chat.ID != t.chatid || msg.MessageID <= 0 || msg.Date <= 0 {
		return nil, errUploadDeleteRef
	}
	deleteRef, err := json.Marshal(deleteReference{
		Version:   1,
		BotID:     t.bot.Self.ID,
		ChatID:    msg.Chat.ID,
		MessageID: msg.MessageID,
	})
	if err != nil {
		return nil, fmt.Errorf("encode Telegram delete reference: %w", err)
	}
	return &blockio.UploadResult{
		FileKey:    msg.Document.FileID,
		DeleteRef:  string(deleteRef),
		UploadedAt: int64(msg.Date) * 1000,
	}, nil
}

func (t *tgBlockIO) waitForUploadSlot(ctx context.Context) error {
	wait := time.Until(t.lastUploadStart.Add(t.uploadMinInterval))
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("wait for Telegram upload slot: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}

type deleteReference struct {
	Version   int   `json:"v"`
	BotID     int64 `json:"bot_id"`
	ChatID    int64 `json:"chat_id"`
	MessageID int   `json:"message_id"`
}

type deleteAPIResponse struct {
	OK         bool `json:"ok"`
	Result     bool `json:"result"`
	ErrorCode  int  `json:"error_code"`
	Parameters struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

type DeleteError struct {
	StatusCode int
	RetryAfter time.Duration
}

type deleteTransportError struct {
	cause error
}

type operationError struct {
	operation string
	cause     error
}

func (e *operationError) Error() string {
	return "Telegram " + e.operation + " failed"
}

func (e *operationError) Unwrap() error {
	return e.cause
}

func (e *deleteTransportError) Error() string {
	return "Telegram delete transport failed"
}

func (e *deleteTransportError) Unwrap() error {
	return e.cause
}

func (e *DeleteError) Error() string {
	return fmt.Sprintf("Telegram delete request failed with status %d", e.StatusCode)
}

func (e *DeleteError) DeleteStatusCode() int {
	return e.StatusCode
}

func (e *DeleteError) DeleteRetryAfter() time.Duration {
	return e.RetryAfter
}

func (t *tgBlockIO) DeleteBlocks(ctx context.Context, deleteRefs []string) error {
	if len(deleteRefs) == 0 {
		return nil
	}
	if len(deleteRefs) > maxDeleteBatchSize {
		return fmt.Errorf("%w: maximum %d messages", errDeleteBatchTooLarge, maxDeleteBatchSize)
	}
	messageIDs, err := t.decodeDeleteReferences(deleteRefs)
	if err != nil {
		return err
	}
	t.deleteMu.Lock()
	defer t.deleteMu.Unlock()
	if err := t.waitForDeleteSlot(ctx); err != nil {
		return err
	}
	t.lastDeleteStart = time.Now()
	return t.requestDeleteMessages(ctx, messageIDs)
}

func (t *tgBlockIO) decodeDeleteReferences(deleteRefs []string) ([]int, error) {
	messageIDs := make([]int, 0, len(deleteRefs))
	for _, raw := range deleteRefs {
		ref, err := t.decodeDeleteReference(raw)
		if err != nil {
			return nil, err
		}
		messageIDs = append(messageIDs, ref.MessageID)
	}
	return messageIDs, nil
}

func (t *tgBlockIO) waitForDeleteSlot(ctx context.Context) error {
	if wait := time.Until(t.lastDeleteStart.Add(time.Second)); wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for Telegram delete slot: %w", ctx.Err())
		case <-timer.C:
		}
	}
	return nil
}

func (t *tgBlockIO) requestDeleteMessages(ctx context.Context, messageIDs []int) error {
	payload, err := json.Marshal(map[string]any{
		"chat_id":     t.chatid,
		"message_ids": messageIDs,
	})
	if err != nil {
		return fmt.Errorf("encode Telegram delete request: %w", err)
	}
	url := fmt.Sprintf(t.endpoint, t.token, "deleteMessages")
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("create Telegram delete request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := t.client.Do(request)
	if err != nil {
		return &deleteTransportError{cause: err}
	}
	defer func() {
		_ = response.Body.Close()
	}()
	var result deleteAPIResponse
	decoder := json.NewDecoder(io.LimitReader(response.Body, 64*1024))
	if err := decoder.Decode(&result); err != nil {
		if response.StatusCode/100 != 2 {
			return &DeleteError{StatusCode: response.StatusCode}
		}
		return fmt.Errorf("decode Telegram delete response: %w", err)
	}
	if response.StatusCode/100 != 2 || !result.OK || !result.Result {
		status := result.ErrorCode
		if status == 0 {
			status = response.StatusCode
		}
		return &DeleteError{StatusCode: status, RetryAfter: time.Duration(result.Parameters.RetryAfter) * time.Second}
	}
	return nil
}

func (t *tgBlockIO) decodeDeleteReference(raw string) (*deleteReference, error) {
	if raw == "" || len(raw) > maxDeleteReferenceSize {
		return nil, errDeleteRefSize
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	var ref deleteReference
	if err := decoder.Decode(&ref); err != nil {
		return nil, fmt.Errorf("decode Telegram delete reference: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errDeleteRefTrailing
	}
	if ref.Version != 1 || ref.BotID == 0 || ref.BotID != t.bot.Self.ID ||
		ref.ChatID == 0 || ref.ChatID != t.chatid || ref.MessageID <= 0 {
		return nil, errDeleteRefIdentity
	}
	return &ref, nil
}

func isRetryableError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var telegramError *tgbotapi.Error
	if errors.As(err, &telegramError) {
		return isRetryableStatus(telegramError.Code)
	}
	var networkError net.Error
	return errors.As(err, &networkError) && networkError.Timeout()
}

func isRetryableStatus(status int) bool {
	return status == http.StatusTooManyRequests ||
		status == http.StatusInternalServerError ||
		status == http.StatusBadGateway ||
		status == http.StatusServiceUnavailable ||
		status == http.StatusGatewayTimeout
}

func waitBeforeRetry(ctx context.Context, attempt int) error {
	timer := time.NewTimer(time.Duration(attempt+1) * defaultRetryDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("wait before Telegram retry: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}

func handleDownloadResponse(response *http.Response, pos int64) (io.ReadCloser, bool, error) {
	if response.StatusCode/100 == 2 {
		if pos == 0 || response.Header.Get("Content-Range") != "" {
			return response.Body, false, nil
		}
		closeErr := response.Body.Close()
		return nil, false, errors.Join(ErrRangeUnsupported, closeErr)
	}

	closeErr := response.Body.Close()
	statusErr := fmt.Errorf("%w: %d", ErrUnexpectedHTTPStatus, response.StatusCode)
	return nil, isRetryableStatus(response.StatusCode), errors.Join(statusErr, closeErr)
}

func (t *tgBlockIO) cacheGetDownloadLink(ctx context.Context, filekey string) (string, error) {
	if lnk, ok := t.linkCache.Get(filekey); ok {
		return lnk, nil
	}

	var lastError error
	for attempt := 0; attempt < defaultRetryCount; attempt++ {
		cf := tgbotapi.FileConfig{FileID: filekey}
		file, err := t.bot.GetFile(cf)
		if err == nil {
			link := file.Link(t.bot.Token)
			_ = t.linkCache.Add(filekey, link)
			return link, nil
		}
		lastError = err
		if !isRetryableError(err) || attempt == defaultRetryCount-1 {
			break
		}
		if err := waitBeforeRetry(ctx, attempt); err != nil {
			return "", err
		}
	}
	return "", &operationError{operation: "file lookup", cause: lastError}
}

func (t *tgBlockIO) Download(ctx context.Context, filekey string, pos int64) (io.ReadCloser, error) {
	link, err := t.cacheGetDownloadLink(ctx, filekey)
	if err != nil {
		return nil, fmt.Errorf("resolve Telegram download link: %w", err)
	}

	var lastError error
	for attempt := 0; attempt < defaultRetryCount; attempt++ {
		body, retry, attemptErr := t.downloadAttempt(ctx, link, pos)
		if attemptErr == nil {
			return body, nil
		}
		lastError = attemptErr
		if !retry || attempt == defaultRetryCount-1 {
			return nil, lastError
		}
		if err := waitBeforeRetry(ctx, attempt); err != nil {
			return nil, fmt.Errorf("retry Telegram download: %w", err)
		}
	}
	return nil, fmt.Errorf("download Telegram block: %w", lastError)
}

func (t *tgBlockIO) downloadAttempt(
	ctx context.Context,
	link string,
	pos int64,
) (io.ReadCloser, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return nil, false, &operationError{operation: "download request creation", cause: err}
	}
	if pos != 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", pos))
	}
	response, err := t.client.Do(req)
	if err != nil {
		return nil, isRetryableError(err), &operationError{operation: "download transport", cause: err}
	}
	return handleDownloadResponse(response, pos)
}

func (t *tgBlockIO) Name() string {
	return "telegram"
}

func create(args any) (blockio.IBlockIO, error) {
	c := &config{}
	if err := utils.ConvStructJson(args, c); err != nil {
		return nil, fmt.Errorf("decode Telegram config: %w", err)
	}
	interval := time.Duration(c.UploadMinIntervalMS) * time.Millisecond
	return New(c.Chatid, c.Token, interval)
}

func init() {
	blockio.Register("telegram", create)
}
