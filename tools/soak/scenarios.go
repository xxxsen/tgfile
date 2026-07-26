package main

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const multipartMinimumPartSize = 5 * 1024 * 1024

var (
	errMultipartResponse = errors.New("invalid multipart response")
	errMissingLockToken  = errors.New("LOCK response did not include a token")
)

type initiatedMultipart struct {
	UploadID string `xml:"UploadId"`
}

type completedMultipart struct {
	XMLName xml.Name             `xml:"CompleteMultipartUpload"`
	Parts   []completedPartEntry `xml:"Part"`
}

type completedPartEntry struct {
	PartNumber int    `xml:"PartNumber"`
	ETag       string `xml:"ETag"`
}

func (r *soakRunner) runPreflight(ctx context.Context) error {
	r.sampleResources()
	if err := r.runBoundaryLifecycle(ctx); err != nil {
		return err
	}
	if err := r.runMultipartLifecycle(ctx, "preflight-multipart.bin"); err != nil {
		return err
	}
	if err := r.runSlowNetworkLifecycle(ctx, "preflight-slow-network.bin", 3); err != nil {
		return err
	}
	return r.runSyncReport(ctx)
}

func (r *soakRunner) runBoundaryLifecycle(ctx context.Context) error {
	key := "preflight-20m-plus-one.bin"
	content := contentFor(r.seed, 1, localBlockSize+1)
	r.trackKey(key)
	_, err := r.expectS3Status(ctx, http.MethodPut, key, "", content, nil, http.StatusOK)
	if err != nil {
		return err
	}
	result, err := r.expectS3Status(ctx, http.MethodHead, key, "", nil, nil, http.StatusOK)
	if err != nil {
		return err
	}
	if result.header.Get("Content-Length") != strconv.Itoa(len(content)) {
		return fmt.Errorf("%w: boundary HEAD length", errResponseMismatch)
	}
	rangeHeader := make(http.Header)
	rangeHeader.Set(
		"Range",
		fmt.Sprintf("bytes=%d-%d", localBlockSize-2, localBlockSize),
	)
	result, err = r.expectS3Status(
		ctx,
		http.MethodGet,
		key,
		"",
		nil,
		rangeHeader,
		http.StatusPartialContent,
	)
	if err != nil {
		return err
	}
	if err := expectBody(result, content[localBlockSize-2:localBlockSize+1]); err != nil {
		return err
	}
	result, err = r.expectWebDAVStatus(ctx, http.MethodGet, key, nil, nil, http.StatusOK)
	if err != nil {
		return err
	}
	if err := expectBody(result, content); err != nil {
		return err
	}
	_, err = r.expectWebDAVStatus(ctx, http.MethodDelete, key, nil, nil, http.StatusNoContent)
	if err != nil {
		return err
	}
	r.untrackKey(key)
	return r.expectS3Missing(ctx, key)
}

func (r *soakRunner) runS3Lifecycle(
	ctx context.Context,
	workerID int,
	cycleID uint64,
) error {
	key := objectKey("s3", workerID, cycleID)
	content := cycleContent(r.seed, cycleID)
	r.trackKey(key)
	_, err := r.expectS3Status(ctx, http.MethodPut, key, "", content, nil, http.StatusOK)
	if err != nil {
		return err
	}
	result, err := r.expectS3Status(ctx, http.MethodHead, key, "", nil, nil, http.StatusOK)
	if err != nil {
		return err
	}
	if result.header.Get("Content-Length") != strconv.Itoa(len(content)) {
		return fmt.Errorf("%w: S3 HEAD length", errResponseMismatch)
	}
	listQuery := url.Values{}
	listQuery.Set("list-type", "2")
	listQuery.Set("prefix", key)
	result, err = r.expectS3Status(ctx, http.MethodGet, "", listQuery.Encode(), nil, nil, http.StatusOK)
	if err != nil {
		return err
	}
	if !strings.Contains(string(result.body), "<Key>"+key+"</Key>") {
		return fmt.Errorf("%w: S3 LIST key", errResponseMismatch)
	}
	result, err = r.expectWebDAVStatus(ctx, http.MethodGet, key, nil, nil, http.StatusOK)
	if err != nil {
		return err
	}
	if err := expectBody(result, content); err != nil {
		return err
	}
	updated := contentFor(r.seed+1, cycleID, max(1, len(content)/2))
	_, err = r.expectWebDAVStatus(ctx, http.MethodPut, key, updated, nil, http.StatusNoContent)
	if err != nil {
		return err
	}
	result, err = r.expectS3Status(ctx, http.MethodGet, key, "", nil, nil, http.StatusOK)
	if err != nil {
		return err
	}
	if err := expectBody(result, updated); err != nil {
		return err
	}
	_, err = r.expectS3Status(ctx, http.MethodDelete, key, "", nil, nil, http.StatusNoContent)
	if err != nil {
		return err
	}
	r.untrackKey(key)
	return r.expectWebDAVMissing(ctx, key)
}

func (r *soakRunner) runWebDAVLifecycle(
	ctx context.Context,
	workerID int,
	cycleID uint64,
) error {
	source := objectKey("dav", workerID, cycleID)
	copied := objectKey("copy", workerID, cycleID)
	moved := objectKey("move", workerID, cycleID)
	content := cycleContent(r.seed+2, cycleID)
	r.trackKey(source)
	if err := r.createAndInspectWebDAVSource(ctx, source, content, cycleID); err != nil {
		return err
	}
	r.trackKey(copied)
	copyHeaders := make(http.Header)
	copyHeaders.Set("Destination", r.webDAVObjectURL(copied))
	_, err := r.expectWebDAVStatus(ctx, "COPY", source, nil, copyHeaders, http.StatusCreated)
	if err != nil {
		return err
	}
	r.trackKey(moved)
	moveHeaders := make(http.Header)
	moveHeaders.Set("Destination", r.webDAVObjectURL(moved))
	_, err = r.expectWebDAVStatus(ctx, "MOVE", copied, nil, moveHeaders, http.StatusCreated)
	if err != nil {
		return err
	}
	r.untrackKey(copied)
	result, err := r.expectS3Status(ctx, http.MethodGet, moved, "", nil, nil, http.StatusOK)
	if err != nil {
		return err
	}
	if err := expectBody(result, content); err != nil {
		return err
	}
	for _, key := range []string{source, moved} {
		_, err = r.expectWebDAVStatus(ctx, http.MethodDelete, key, nil, nil, http.StatusNoContent)
		if err != nil {
			return err
		}
		r.untrackKey(key)
		if err := r.expectS3Missing(ctx, key); err != nil {
			return err
		}
	}
	return nil
}

func (r *soakRunner) createAndInspectWebDAVSource(
	ctx context.Context,
	source string,
	content []byte,
	cycleID uint64,
) error {
	_, err := r.expectWebDAVStatus(ctx, http.MethodPut, source, content, nil, http.StatusCreated)
	if err != nil {
		return err
	}
	propertyBody := []byte(
		`<D:propertyupdate xmlns:D="DAV:" xmlns:S="urn:tgfile:soak">` +
			`<D:set><D:prop><S:cycle>` + strconv.FormatUint(cycleID, 10) +
			`</S:cycle></D:prop></D:set></D:propertyupdate>`,
	)
	_, err = r.expectWebDAVStatus(
		ctx,
		"PROPPATCH",
		source,
		propertyBody,
		http.Header{"Content-Type": []string{"application/xml"}},
		http.StatusMultiStatus,
	)
	if err != nil {
		return err
	}
	propfindBody := []byte(
		`<D:propfind xmlns:D="DAV:" xmlns:S="urn:tgfile:soak">` +
			`<D:prop><S:cycle/></D:prop></D:propfind>`,
	)
	propfindHeaders := make(http.Header)
	propfindHeaders.Set("Depth", "0")
	propfindHeaders.Set("Content-Type", "application/xml")
	result, err := r.expectWebDAVStatus(
		ctx,
		"PROPFIND",
		source,
		propfindBody,
		propfindHeaders,
		http.StatusMultiStatus,
	)
	if err != nil {
		return err
	}
	if !strings.Contains(string(result.body), strconv.FormatUint(cycleID, 10)) {
		return fmt.Errorf("%w: WebDAV dead property", errResponseMismatch)
	}
	result, err = r.expectS3Status(ctx, http.MethodGet, source, "", nil, nil, http.StatusOK)
	if err != nil {
		return err
	}
	return expectBody(result, content)
}

func (r *soakRunner) runLockLifecycle(
	ctx context.Context,
	workerID int,
	cycleID uint64,
) error {
	key := objectKey("lock", workerID, cycleID)
	content := cycleContent(r.seed+3, cycleID)
	r.trackKey(key)
	_, err := r.expectWebDAVStatus(ctx, http.MethodPut, key, content, nil, http.StatusCreated)
	if err != nil {
		return err
	}
	lockBody := []byte(
		`<D:lockinfo xmlns:D="DAV:"><D:lockscope><D:exclusive/></D:lockscope>` +
			`<D:locktype><D:write/></D:locktype></D:lockinfo>`,
	)
	lockHeaders := make(http.Header)
	lockHeaders.Set("Depth", "0")
	lockHeaders.Set("Timeout", "Second-120")
	result, err := r.expectWebDAVStatus(ctx, "LOCK", key, lockBody, lockHeaders, http.StatusOK)
	if err != nil {
		return err
	}
	lockToken := result.header.Get("Lock-Token")
	if lockToken == "" {
		return errMissingLockToken
	}
	updated := contentFor(r.seed+4, cycleID, max(1, len(content)))
	_, err = r.expectWebDAVStatus(ctx, http.MethodPut, key, updated, nil, http.StatusLocked)
	if err != nil {
		return err
	}
	conditionalHeaders := make(http.Header)
	conditionalHeaders.Set("If", "("+lockToken+")")
	_, err = r.expectWebDAVStatus(ctx, http.MethodPut, key, updated, conditionalHeaders, http.StatusNoContent)
	if err != nil {
		return err
	}
	unlockHeaders := make(http.Header)
	unlockHeaders.Set("Lock-Token", lockToken)
	_, err = r.expectWebDAVStatus(ctx, "UNLOCK", key, nil, unlockHeaders, http.StatusNoContent)
	if err != nil {
		return err
	}
	result, err = r.expectS3Status(ctx, http.MethodGet, key, "", nil, nil, http.StatusOK)
	if err != nil {
		return err
	}
	if err := expectBody(result, updated); err != nil {
		return err
	}
	_, err = r.expectWebDAVStatus(ctx, http.MethodDelete, key, nil, nil, http.StatusNoContent)
	if err != nil {
		return err
	}
	r.untrackKey(key)
	return nil
}

func (r *soakRunner) runSlowNetworkLifecycle(
	ctx context.Context,
	key string,
	cycleID uint64,
) error {
	r.slowCycles.Add(1)
	content := contentFor(r.seed+7, cycleID, 2*1024*1024)
	r.trackKey(key)
	result, err := r.doS3SlowUpload(ctx, key, content)
	if err != nil {
		return err
	}
	if err := expectStatus(result, http.StatusOK); err != nil {
		return err
	}
	result, err = r.doWebDAVSlowDownload(ctx, key)
	if err != nil {
		return err
	}
	if err := expectStatus(result, http.StatusOK); err != nil {
		return err
	}
	if err := expectBody(result, content); err != nil {
		return err
	}
	updated := contentFor(r.seed+8, cycleID, 1024*1024)
	result, err = r.doWebDAVSlowUpload(ctx, key, updated)
	if err != nil {
		return err
	}
	if err := expectStatus(result, http.StatusNoContent); err != nil {
		return err
	}
	result, err = r.doS3SlowDownload(ctx, key)
	if err != nil {
		return err
	}
	if err := expectStatus(result, http.StatusOK); err != nil {
		return err
	}
	if err := expectBody(result, updated); err != nil {
		return err
	}
	_, err = r.expectS3Status(ctx, http.MethodDelete, key, "", nil, nil, http.StatusNoContent)
	if err != nil {
		return err
	}
	r.untrackKey(key)
	return r.expectWebDAVMissing(ctx, key)
}

func (r *soakRunner) runMultipartLifecycle(ctx context.Context, key string) error {
	r.multiCycles.Add(1)
	r.trackKey(key)
	result, err := r.expectS3Status(ctx, http.MethodPost, key, "uploads", nil, nil, http.StatusOK)
	if err != nil {
		return err
	}
	var initiated initiatedMultipart
	if err := xml.Unmarshal(result.body, &initiated); err != nil {
		return fmt.Errorf("decode multipart initiation: %w", err)
	}
	if len(initiated.UploadID) != 64 {
		return fmt.Errorf("%w: upload ID length %d", errMultipartResponse, len(initiated.UploadID))
	}
	r.trackUpload(initiated.UploadID, key)
	first := contentFor(r.seed+5, 1, multipartMinimumPartSize)
	second := contentFor(r.seed+6, 2, 4097)
	etags := make([]string, 0, 2)
	for index, content := range [][]byte{first, second} {
		query := url.Values{}
		query.Set("partNumber", strconv.Itoa(index+1))
		query.Set("uploadId", initiated.UploadID)
		result, err = r.expectS3Status(ctx, http.MethodPut, key, query.Encode(), content, nil, http.StatusOK)
		if err != nil {
			return err
		}
		etag := result.header.Get("ETag")
		if etag == "" {
			return fmt.Errorf("%w: part %d ETag", errMultipartResponse, index+1)
		}
		etags = append(etags, etag)
	}
	completeBody, err := xml.Marshal(completedMultipart{Parts: []completedPartEntry{
		{PartNumber: 1, ETag: etags[0]},
		{PartNumber: 2, ETag: etags[1]},
	}})
	if err != nil {
		return fmt.Errorf("encode multipart completion: %w", err)
	}
	completeQuery := url.Values{}
	completeQuery.Set("uploadId", initiated.UploadID)
	_, err = r.expectS3Status(ctx, http.MethodPost, key, completeQuery.Encode(), completeBody, nil, http.StatusOK)
	if err != nil {
		return err
	}
	r.untrackUpload(initiated.UploadID)
	expected := append([]byte{}, first...)
	expected = append(expected, second...)
	result, err = r.expectWebDAVStatus(ctx, http.MethodGet, key, nil, nil, http.StatusOK)
	if err != nil {
		return err
	}
	if err := expectBody(result, expected); err != nil {
		return err
	}
	_, err = r.expectWebDAVStatus(ctx, http.MethodDelete, key, nil, nil, http.StatusNoContent)
	if err != nil {
		return err
	}
	r.untrackKey(key)
	return r.expectS3Missing(ctx, key)
}

func (r *soakRunner) runSyncReport(ctx context.Context) error {
	body := []byte(
		`<D:sync-collection xmlns:D="DAV:"><D:sync-token/>` +
			`<D:sync-level>1</D:sync-level><D:prop><D:getetag/></D:prop>` +
			`</D:sync-collection>`,
	)
	headers := make(http.Header)
	headers.Set("Depth", "0")
	headers.Set("Content-Type", "application/xml")
	result, err := r.expectWebDAVStatus(
		ctx,
		"REPORT",
		"",
		body,
		headers,
		http.StatusMultiStatus,
	)
	if err != nil {
		return err
	}
	if !strings.Contains(string(result.body), "sync-token") {
		return fmt.Errorf("%w: sync-token missing", errResponseMismatch)
	}
	return nil
}

func (r *soakRunner) expectS3Missing(ctx context.Context, key string) error {
	_, err := r.expectS3Status(
		ctx,
		http.MethodHead,
		key,
		"",
		nil,
		nil,
		http.StatusNotFound,
	)
	if err != nil {
		return err
	}
	return nil
}

func (r *soakRunner) expectWebDAVMissing(ctx context.Context, key string) error {
	_, err := r.expectWebDAVStatus(
		ctx,
		http.MethodHead,
		key,
		nil,
		nil,
		http.StatusNotFound,
	)
	if err != nil {
		return err
	}
	return nil
}

func (r *soakRunner) webDAVObjectURL(key string) string {
	return r.baseURL + "/webdav/" + soakBucket + "/" + url.PathEscape(key)
}

func objectKey(prefix string, workerID int, cycleID uint64) string {
	return fmt.Sprintf("%s-%d-%d.bin", prefix, workerID, cycleID)
}

func cycleContent(seed, cycleID uint64) []byte {
	sizes := [...]int{0, 1, 1024, 64 * 1024, 256 * 1024}
	size := sizes[(cycleID+seed)%uint64(len(sizes))]
	return contentFor(seed, cycleID, size)
}

func contentFor(seed, cycleID uint64, size int) []byte {
	content := make([]byte, size)
	state := seed ^ (cycleID * 0x9e3779b97f4a7c15)
	for index := range content {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		content[index] = byte(state & 0xff)
	}
	return content
}
