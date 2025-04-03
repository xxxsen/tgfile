package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"time"

	"github.com/xxxsen/tgfile/proxyutil"
	"github.com/xxxsen/tgfile/server/model"
)

var (
	defaultHttpClient = &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			IdleConnTimeout:     20 * time.Second,
			MaxIdleConns:        5,
			MaxIdleConnsPerHost: 1,
		},
	}
)

type defaultClient struct {
	c *config
}

func (d *defaultClient) buildUrl(api string) string {
	return fmt.Sprintf("%s://%s%s", d.c.Schema, d.c.Host, api)
}

func (d *defaultClient) callJsonPost(ctx context.Context, api string, in interface{}, out interface{}) error {
	raw, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.buildUrl(api), bytes.NewReader(raw))
	if err != nil {
		return err
	}
	d.applyAuth(req)
	req.Header.Set("Content-Type", "application/json")
	rsp, err := defaultHttpClient.Do(req)
	if err != nil {
		return err
	}
	defer rsp.Body.Close()
	if rsp.StatusCode != http.StatusOK {
		return fmt.Errorf("status code not ok, code:%d", rsp.StatusCode)
	}
	pkgRsp := &proxyutil.CommonResponse{
		Data: out,
	}
	if err := json.NewDecoder(rsp.Body).Decode(pkgRsp); err != nil {
		return err
	}
	if pkgRsp.Code != 0 {
		return fmt.Errorf("biz code not ok, code:%d, msg:%s", pkgRsp.Code, pkgRsp.Message)
	}
	return nil
}

func (d *defaultClient) CreateDraft(ctx context.Context, size int64) (string, int64, error) {
	req := &model.BeginUploadRequest{
		FileSize: size,
	}
	rsp := &model.BeginUploadResponse{}
	if err := d.callJsonPost(ctx, apiCreateDraft, req, rsp); err != nil {
		return "", 0, err
	}
	return rsp.UploadKey, rsp.BlockSize, nil
}

func (d *defaultClient) applyAuth(req *http.Request) {
	if len(d.c.AccessKey) == 0 {
		return
	}
	req.SetBasicAuth(d.c.AccessKey, d.c.SecretKey)
}

func (d *defaultClient) CreatePart(ctx context.Context, uploadKey string, partid int64, r io.Reader) error {
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	filePart, err := writer.CreateFormFile("part_data", "part")
	if err != nil {
		return err
	}
	if _, err := io.Copy(filePart, r); err != nil {
		return err
	}

	// 添加额外的字段 partid
	_ = writer.WriteField("part_id", fmt.Sprintf("%d", partid))
	_ = writer.WriteField("upload_key", uploadKey)
	if err := writer.Close(); err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, d.buildUrl(apiCreatePart), body)
	if err != nil {
		return err
	}
	d.applyAuth(req)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	rsp, err := defaultHttpClient.Do(req)
	if err != nil {
		return err
	}
	defer rsp.Body.Close()
	if rsp.StatusCode != http.StatusOK {
		return fmt.Errorf("status code not ok, code:%d", rsp.StatusCode)
	}
	pkgRsp := &proxyutil.CommonResponse{
		Data: &model.PartUploadResponse{},
	}
	if err := json.NewDecoder(rsp.Body).Decode(pkgRsp); err != nil {
		return err
	}
	if pkgRsp.Code != 0 {
		return fmt.Errorf("biz code not ok, code:%d, msg:%s", pkgRsp.Code, pkgRsp.Message)
	}
	return nil
}

func (d *defaultClient) FinishCreate(ctx context.Context, uploadKey string) (string, error) {
	req := &model.FinishUploadRequest{
		UploadKey: uploadKey,
	}
	rsp := &model.FinishUploadResponse{}
	if err := d.callJsonPost(ctx, apiFinishCreate, req, rsp); err != nil {
		return "", err
	}
	return rsp.FileKey, nil
}

func New(opts ...Option) (IClient, error) {
	c := &config{
		Schema: "https",
	}
	for _, opt := range opts {
		opt(c)
	}
	if len(c.Host) == 0 {
		return nil, fmt.Errorf("no host found")
	}
	return &defaultClient{c: c}, nil
}
