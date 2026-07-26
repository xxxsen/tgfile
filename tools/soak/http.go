package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsv4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

var (
	errUnexpectedHTTPStatus = errors.New("unexpected HTTP status")
	errResponseMismatch     = errors.New("response content mismatch")
)

type httpResult struct {
	status int
	header http.Header
	body   []byte
}

type requestObservation struct {
	elapsed time.Duration
	status  int
	err     error
}

func (r *soakRunner) expectS3Status(
	ctx context.Context,
	method, key, rawQuery string,
	body []byte,
	headers http.Header,
	expected ...int,
) (*httpResult, error) {
	result, err := r.doS3(ctx, method, key, rawQuery, body, headers)
	if err != nil {
		return nil, err
	}
	if err := expectStatus(result, expected...); err != nil {
		return nil, err
	}
	return result, nil
}

func (r *soakRunner) expectWebDAVStatus(
	ctx context.Context,
	method, key string,
	body []byte,
	headers http.Header,
	expected ...int,
) (*httpResult, error) {
	result, err := r.doWebDAV(ctx, method, key, body, headers)
	if err != nil {
		return nil, err
	}
	if err := expectStatus(result, expected...); err != nil {
		return nil, err
	}
	return result, nil
}

func (r *soakRunner) doS3(
	ctx context.Context,
	method, key, rawQuery string,
	body []byte,
	headers http.Header,
) (*httpResult, error) {
	request, err := r.buildS3Request(
		ctx,
		method,
		key,
		rawQuery,
		body,
		bytes.NewReader(body),
		headers,
	)
	if err != nil {
		return nil, err
	}
	return r.doRequest(request, 0, 0)
}

func (r *soakRunner) doS3SlowUpload(
	ctx context.Context,
	key string,
	body []byte,
) (*httpResult, error) {
	request, err := r.buildS3Request(
		ctx,
		http.MethodPut,
		key,
		"",
		body,
		newDelayedReader(ctx, bytes.NewReader(body), r.clientDelay, 8*1024),
		nil,
	)
	if err != nil {
		return nil, err
	}
	return r.doRequest(request, 0, 0)
}

func (r *soakRunner) doS3SlowDownload(
	ctx context.Context,
	key string,
) (*httpResult, error) {
	request, err := r.buildS3Request(
		ctx,
		http.MethodGet,
		key,
		"",
		nil,
		bytes.NewReader(nil),
		nil,
	)
	if err != nil {
		return nil, err
	}
	return r.doRequest(request, r.clientDelay, 8*1024)
}

func (r *soakRunner) buildS3Request(
	ctx context.Context,
	method, key, rawQuery string,
	body []byte,
	reader io.Reader,
	headers http.Header,
) (*http.Request, error) {
	target := r.baseURL + "/" + soakBucket
	if key != "" {
		target += "/" + url.PathEscape(key)
	}
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	request, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, fmt.Errorf("create S3 request: %w", err)
	}
	request.ContentLength = int64(len(body))
	request.Header = headers.Clone()
	if request.Header == nil {
		request.Header = make(http.Header)
	}
	payloadHashBytes := sha256.Sum256(body)
	payloadHash := hex.EncodeToString(payloadHashBytes[:])
	request.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if err := (awsv4.NewSigner()).SignHTTP(
		ctx,
		aws.Credentials{AccessKeyID: soakAccessKey, SecretAccessKey: soakSecretKey},
		request,
		payloadHash,
		"s3",
		"us-east-1",
		time.Now(),
	); err != nil {
		return nil, fmt.Errorf("sign S3 request: %w", err)
	}
	return request, nil
}

func (r *soakRunner) doWebDAV(
	ctx context.Context,
	method, key string,
	body []byte,
	headers http.Header,
) (*httpResult, error) {
	request, err := r.buildWebDAVRequest(
		ctx,
		method,
		key,
		body,
		bytes.NewReader(body),
		headers,
	)
	if err != nil {
		return nil, err
	}
	return r.doRequest(request, 0, 0)
}

func (r *soakRunner) doWebDAVSlowUpload(
	ctx context.Context,
	key string,
	body []byte,
) (*httpResult, error) {
	request, err := r.buildWebDAVRequest(
		ctx,
		http.MethodPut,
		key,
		body,
		newDelayedReader(ctx, bytes.NewReader(body), r.clientDelay, 8*1024),
		nil,
	)
	if err != nil {
		return nil, err
	}
	return r.doRequest(request, 0, 0)
}

func (r *soakRunner) doWebDAVSlowDownload(
	ctx context.Context,
	key string,
) (*httpResult, error) {
	request, err := r.buildWebDAVRequest(
		ctx,
		http.MethodGet,
		key,
		nil,
		bytes.NewReader(nil),
		nil,
	)
	if err != nil {
		return nil, err
	}
	return r.doRequest(request, r.clientDelay, 8*1024)
}

func (r *soakRunner) buildWebDAVRequest(
	ctx context.Context,
	method, key string,
	body []byte,
	reader io.Reader,
	headers http.Header,
) (*http.Request, error) {
	target := r.baseURL + "/webdav/" + soakBucket
	if key != "" {
		target += "/" + url.PathEscape(key)
	}
	request, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, fmt.Errorf("create WebDAV request: %w", err)
	}
	request.ContentLength = int64(len(body))
	request.Header = headers.Clone()
	if request.Header == nil {
		request.Header = make(http.Header)
	}
	request.SetBasicAuth(soakAccessKey, soakSecretKey)
	return request, nil
}

func (r *soakRunner) doRequest(
	request *http.Request,
	readDelay time.Duration,
	readChunkSize int,
) (*httpResult, error) {
	r.requests.Add(1)
	startedAt := time.Now()
	result, err := r.executeRequest(request, readDelay, readChunkSize)
	if r.requestObserver != nil {
		status := httpStatus(result)
		r.requestObserver(requestObservation{
			elapsed: time.Since(startedAt),
			status:  status,
			err:     err,
		})
	}
	return result, err
}

func (r *soakRunner) executeRequest(
	request *http.Request,
	readDelay time.Duration,
	readChunkSize int,
) (*httpResult, error) {
	response, err := r.transport.RoundTrip(request)
	if err != nil {
		return nil, fmt.Errorf("execute %s request: %w", request.Method, err)
	}
	reader := io.Reader(response.Body)
	if readDelay > 0 {
		reader = newDelayedReader(
			request.Context(),
			response.Body,
			readDelay,
			readChunkSize,
		)
	}
	body, err := io.ReadAll(reader)
	closeErr := response.Body.Close()
	if err := errors.Join(err, closeErr); err != nil {
		return nil, fmt.Errorf("read and close %s response: %w", request.Method, err)
	}
	return &httpResult{
		status: response.StatusCode,
		header: response.Header.Clone(),
		body:   body,
	}, nil
}

func httpStatus(result *httpResult) int {
	if result == nil {
		return 0
	}
	return result.status
}

func expectStatus(result *httpResult, expected ...int) error {
	for _, status := range expected {
		if result.status == status {
			return nil
		}
	}
	body := string(result.body)
	if len(body) > 512 {
		body = body[:512]
	}
	return fmt.Errorf(
		"%w: got %d, expected %s, body %q",
		errUnexpectedHTTPStatus,
		result.status,
		statusList(expected),
		body,
	)
}

func expectBody(result *httpResult, expected []byte) error {
	if bytes.Equal(result.body, expected) {
		return nil
	}
	return fmt.Errorf(
		"%w: got %d bytes, expected %d bytes",
		errResponseMismatch,
		len(result.body),
		len(expected),
	)
}

func statusList(statuses []int) string {
	values := make([]string, 0, len(statuses))
	for _, status := range statuses {
		values = append(values, strconv.Itoa(status))
	}
	return strings.Join(values, ",")
}
