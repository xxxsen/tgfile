package telegram

import (
	"context"
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
)

var defaultHTTPClient = &http.Client{
	Transport: &http.Transport{
		Dial: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 10 * time.Second,
		}).Dial,
		IdleConnTimeout: 20 * time.Second,
		MaxIdleConns:    20,
	},
}

type tgBlockIO struct {
	chatid    int64
	token     string
	bot       *tgbotapi.BotAPI
	linkCache *lru.LRU[string, string]
}

func New(chatid int64, token string) (blockio.IBlockIO, error) {
	cache := lru.NewLRU[string, string](defaultMaxFileLinkToCache, nil, defaultMaxFileLinkCacheTTL)
	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		return nil, fmt.Errorf("init bot fail, err:%w", err)
	}
	return &tgBlockIO{
		chatid:    chatid,
		token:     token,
		bot:       bot,
		linkCache: cache,
	}, nil
}

func (t *tgBlockIO) MaxFileSize() int64 {
	return defaultMaxFileSize
}

func (t *tgBlockIO) Upload(ctx context.Context, r io.Reader) (string, error) {
	sname := uuid.NewString()
	freader := tgbotapi.FileReader{
		Name:   sname,
		Reader: r,
	}
	doc := tgbotapi.NewDocument(t.chatid, freader)
	doc.DisableNotification = true
	msg, err := t.bot.Send(doc)
	if err != nil {
		return "", fmt.Errorf("send document fail, err:%w", err)
	}

	return msg.Document.FileID, nil
}

func (t *tgBlockIO) cacheGetDownloadLink(filekey string) (string, error) {
	if lnk, ok := t.linkCache.Get(filekey); ok {
		return lnk, nil
	}
	cf := tgbotapi.FileConfig{FileID: filekey}
	f, err := t.bot.GetFile(cf)
	if err != nil {
		return "", err
	}
	lnk := f.Link(t.bot.Token)
	_ = t.linkCache.Add(filekey, lnk)
	return lnk, nil
}

func (t *tgBlockIO) Download(ctx context.Context, filekey string, pos int64) (io.ReadCloser, error) {
	link, err := t.cacheGetDownloadLink(filekey)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return nil, fmt.Errorf("create http request fail, err:%w", err)
	}
	if pos != 0 {
		rangeHeader := fmt.Sprintf("bytes=%d-", pos)
		req.Header.Set("Range", rangeHeader)
	}
	rsp, err := defaultHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do http request fail, err:%w", err)
	}
	//caller should close rsp.Body
	if rsp.StatusCode/100 != 2 {
		rsp.Body.Close()
		return nil, fmt.Errorf("status code not ok, code:%d", rsp.StatusCode)
	}
	if pos != 0 && len(rsp.Header.Get("Content-Range")) == 0 {
		rsp.Body.Close()
		return nil, fmt.Errorf("not support range")
	}
	return rsp.Body, nil
}

func (t *tgBlockIO) Name() string {
	return "telegram"
}

func create(args interface{}) (blockio.IBlockIO, error) {
	c := &config{}
	if err := utils.ConvStructJson(args, c); err != nil {
		return nil, err
	}
	return New(c.Chatid, c.Token)
}

func init() {
	blockio.Register("telegram", create)
}
