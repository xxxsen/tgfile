package telegram

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
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
)

var (
	ErrRangeUnsupported     = errors.New("download response does not support range")
	ErrUnexpectedHTTPStatus = errors.New("unexpected HTTP status")

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
	chatid    int64
	token     string
	bot       *tgbotapi.BotAPI
	client    *http.Client
	linkCache *lru.LRU[string, string]
}

func New(chatid int64, token string) (blockio.IBlockIO, error) {
	return newWithEndpoint(chatid, token, tgbotapi.APIEndpoint, defaultHTTPClient)
}

func newWithEndpoint(chatid int64, token, endpoint string, client *http.Client) (blockio.IBlockIO, error) {
	cache := lru.NewLRU[string, string](defaultMaxFileLinkToCache, nil, defaultMaxFileLinkCacheTTL)
	bot, err := tgbotapi.NewBotAPIWithClient(token, endpoint, client)
	if err != nil {
		return nil, fmt.Errorf("init bot fail, err:%w", err)
	}
	return &tgBlockIO{
		chatid:    chatid,
		token:     token,
		bot:       bot,
		client:    client,
		linkCache: cache,
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

func (t *tgBlockIO) Upload(ctx context.Context, r io.Reader) (string, error) {
	sname := uuid.NewString()
	freader := tgbotapi.FileReader{
		Name:   sname,
		Reader: &contextReader{ctx: ctx, reader: r},
	}
	doc := tgbotapi.NewDocument(t.chatid, freader)
	doc.DisableNotification = true
	msg, err := t.bot.Send(doc)
	if err != nil {
		return "", fmt.Errorf("send document fail, err:%w", err)
	}

	return msg.Document.FileID, nil
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
	return "", fmt.Errorf("get Telegram download link: %w", lastError)
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
		return nil, false, fmt.Errorf("create HTTP download request: %w", err)
	}
	if pos != 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", pos))
	}
	response, err := t.client.Do(req)
	if err != nil {
		return nil, isRetryableError(err), fmt.Errorf("perform HTTP download request: %w", err)
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
	return New(c.Chatid, c.Token)
}

func init() {
	blockio.Register("telegram", create)
}
