package webdav

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/xxxsen/tgfile/entity"
	"github.com/xxxsen/tgfile/filemgr"
	"github.com/xxxsen/tgfile/server/httpkit"
)

func (h *WebdavHandler) handleOption(c *gin.Context) {
	setPrivateDAVHeaders(c.Writer.Header())
	c.Header("Allow", strings.Join(h.allowedMethods(c), ", "))
	c.Header("DAV", "1, 2, sync-collection")
	c.Header("MS-Author-Via", "DAV")
	c.Status(http.StatusOK)
}

func (h *WebdavHandler) handleGet(c *gin.Context) {
	h.handleRead(c, false)
}

func (h *WebdavHandler) handleHead(c *gin.Context) {
	h.handleRead(c, true)
}

func (h *WebdavHandler) handleRead(c *gin.Context, head bool) {
	item, ok := h.stat(c)
	if !ok {
		return
	}
	if item.IsDir {
		h.writeError(c, http.StatusMethodNotAllowed, errDirectoryStream, "")
		return
	}
	h.setValidatorHeaders(c, item)
	status, err := evaluateReadPreconditions(c.Request, item)
	if err != nil {
		h.writeError(c, http.StatusBadRequest, err, "")
		return
	}
	if status != 0 {
		c.Status(status)
		return
	}
	h.setRepresentationHeaders(c, item)
	if head {
		c.Status(http.StatusOK)
		return
	}
	stream, err := h.fmgr.OpenFile(c.Request.Context(), item.FileId)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			h.writeError(c, http.StatusNotFound, err, "")
			return
		}
		h.writeMappedError(c, fmt.Errorf("open WebDAV stream: %w", err))
		return
	}
	defer logCloseError(c.Request.Context(), stream, "close WebDAV download")
	http.ServeContent(
		c.Writer,
		c.Request,
		item.FileName,
		time.UnixMilli(item.Mtime).UTC(),
		stream,
	)
}

func (h *WebdavHandler) handlePut(c *gin.Context) {
	length := c.Request.ContentLength
	if length < 0 {
		h.writeError(c, http.StatusLengthRequired, errPUTLengthUnknown, "")
		return
	}
	if h.maxUploadSize > 0 && length > h.maxUploadSize {
		h.writeError(
			c,
			http.StatusRequestEntityTooLarge,
			errPUTTooLarge,
			"",
		)
		return
	}
	condition, err := h.requestCondition(c)
	if err != nil {
		h.writeMappedError(c, err)
		return
	}
	ctx := c.Request.Context()
	fileID, err := h.fmgr.CreateFile(ctx, length, c.Request.Body)
	if err != nil {
		h.writeMappedError(c, fmt.Errorf("create WebDAV file: %w", err))
		return
	}
	result, publishErr := h.fmgr.PublishWebDAVFile(
		ctx,
		h.buildSrcPath(c),
		fileID,
		length,
		h.mutationOptions(c, condition),
	)
	if publishErr != nil {
		cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		cleanupErr := h.fmgr.DiscardUnpublishedFile(cleanupContext, fileID)
		cancel()
		h.writeMappedError(c, errors.Join(publishErr, cleanupErr))
		return
	}
	h.setValidatorHeaders(c, result.Link)
	if result.Created {
		c.Status(http.StatusCreated)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *WebdavHandler) handleDelete(c *gin.Context) {
	item, ok := h.stat(c)
	if !ok {
		return
	}
	if item.IsDir {
		depth := c.GetHeader("Depth")
		if depth != "" && depth != "infinity" {
			h.writeMappedError(c, errInvalidDepth)
			return
		}
	}
	condition, err := h.requestCondition(c)
	if err != nil {
		h.writeMappedError(c, err)
		return
	}
	if err := h.fmgr.DeleteWebDAVResource(
		c.Request.Context(),
		h.buildSrcPath(c),
		h.mutationOptions(c, condition),
	); err != nil {
		h.writeMappedError(c, err)
		return
	}
	setPrivateDAVHeaders(c.Writer.Header())
	c.Status(http.StatusNoContent)
}

func (h *WebdavHandler) handleMkcol(c *gin.Context) {
	if c.Request.ContentLength != 0 || c.GetHeader("Content-Type") != "" {
		h.writeError(c, http.StatusUnsupportedMediaType, errMKCOLBody, "")
		return
	}
	condition, err := h.requestCondition(c)
	if err != nil {
		h.writeMappedError(c, err)
		return
	}
	if _, err := h.fmgr.CreateWebDAVCollection(
		c.Request.Context(),
		h.buildSrcPath(c),
		h.mutationOptions(c, condition),
	); err != nil {
		h.writeMappedError(c, err)
		return
	}
	setPrivateDAVHeaders(c.Writer.Header())
	c.Status(http.StatusCreated)
}

func (h *WebdavHandler) handleCopy(c *gin.Context) {
	sourceItem, ok := h.stat(c)
	if !ok {
		return
	}
	recursive := true
	depth := c.GetHeader("Depth")
	if sourceItem.IsDir {
		switch depth {
		case "", "infinity":
		case "0":
			recursive = false
		default:
			h.writeMappedError(c, errInvalidDepth)
			return
		}
	} else if depth != "" && depth != "0" && depth != "infinity" {
		h.writeMappedError(c, errInvalidDepth)
		return
	}
	overwrite, err := parseOverwrite(c.GetHeader("Overwrite"))
	if err != nil {
		h.writeMappedError(c, err)
		return
	}
	destination, err := h.tryBuildDstPath(c)
	if err != nil {
		h.writeMappedError(c, err)
		return
	}
	condition, err := h.requestCondition(c)
	if err != nil {
		h.writeMappedError(c, err)
		return
	}
	result, err := h.fmgr.CopyWebDAVResource(
		c.Request.Context(),
		h.buildSrcPath(c),
		destination,
		overwrite,
		recursive,
		h.mutationOptions(c, condition),
	)
	if err != nil {
		h.writeMappedError(c, err)
		return
	}
	setPrivateDAVHeaders(c.Writer.Header())
	if result.Created {
		c.Status(http.StatusCreated)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *WebdavHandler) handleMove(c *gin.Context) {
	sourceItem, ok := h.stat(c)
	if !ok {
		return
	}
	if sourceItem.IsDir {
		depth := c.GetHeader("Depth")
		if depth != "" && depth != "infinity" {
			h.writeMappedError(c, errInvalidDepth)
			return
		}
	}
	overwrite, err := parseOverwrite(c.GetHeader("Overwrite"))
	if err != nil {
		h.writeMappedError(c, err)
		return
	}
	destination, err := h.tryBuildDstPath(c)
	if err != nil {
		h.writeMappedError(c, err)
		return
	}
	condition, err := h.requestCondition(c)
	if err != nil {
		h.writeMappedError(c, err)
		return
	}
	result, err := h.fmgr.MoveWebDAVResource(
		c.Request.Context(),
		h.buildSrcPath(c),
		destination,
		overwrite,
		h.mutationOptions(c, condition),
	)
	if err != nil {
		h.writeMappedError(c, err)
		return
	}
	setPrivateDAVHeaders(c.Writer.Header())
	if result.Created {
		c.Status(http.StatusCreated)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *WebdavHandler) setRepresentationHeaders(c *gin.Context, item *entity.FileLinkMeta) {
	c.Header("Content-Type", httpkit.DetermineMimeType(item.FileName))
	c.Header("Content-Length", strconv.FormatInt(item.FileSize, 10))
	c.Header("Accept-Ranges", "bytes")
}

func (h *WebdavHandler) setValidatorHeaders(c *gin.Context, item *entity.FileLinkMeta) {
	setPrivateDAVHeaders(c.Writer.Header())
	c.Header("ETag", filemgr.WebDAVETag(item))
	c.Header("Last-Modified", time.UnixMilli(item.Mtime).UTC().Format(http.TimeFormat))
}

func evaluateReadPreconditions(request *http.Request, item *entity.FileLinkMeta) (int, error) {
	status, err := evaluateIfMatchPrecondition(request, item)
	if err != nil || status != 0 {
		return status, err
	}
	return evaluateIfNoneMatchPrecondition(request, item)
}

func evaluateIfMatchPrecondition(request *http.Request, item *entity.FileLinkMeta) (int, error) {
	etag := filemgr.WebDAVETag(item)
	ifMatch := request.Header.Get("If-Match")
	if err := validateEntityTagHeader(ifMatch); err != nil {
		return 0, err
	}
	if ifMatch != "" {
		return evaluateETagPrecondition(
			ifMatch,
			etag,
			false,
			false,
			http.StatusPreconditionFailed,
		)
	}
	return evaluateDatePrecondition(
		request.Header.Get("If-Unmodified-Since"),
		item.Mtime,
		false,
		http.StatusPreconditionFailed,
	)
}

func evaluateIfNoneMatchPrecondition(request *http.Request, item *entity.FileLinkMeta) (int, error) {
	etag := filemgr.WebDAVETag(item)
	ifNoneMatch := request.Header.Get("If-None-Match")
	if err := validateEntityTagHeader(ifNoneMatch); err != nil {
		return 0, err
	}
	if ifNoneMatch != "" {
		return evaluateETagPrecondition(
			ifNoneMatch,
			etag,
			true,
			true,
			http.StatusNotModified,
		)
	}
	return evaluateDatePrecondition(
		request.Header.Get("If-Modified-Since"),
		item.Mtime,
		true,
		http.StatusNotModified,
	)
}

func evaluateETagPrecondition(
	value, etag string,
	weak, failOnMatch bool,
	failureStatus int,
) (int, error) {
	matches, err := matchesETagHeader(value, etag, weak)
	if err != nil {
		return 0, err
	}
	if matches == failOnMatch {
		return failureStatus, nil
	}
	return 0, nil
}

func evaluateDatePrecondition(
	value string,
	mtime int64,
	failWhenNotModified bool,
	failureStatus int,
) (int, error) {
	if value == "" {
		return 0, nil
	}
	limit, err := http.ParseTime(value)
	if err != nil {
		return 0, fmt.Errorf("parse HTTP precondition date: %w", err)
	}
	modifiedAfter := time.UnixMilli(mtime).
		Truncate(time.Second).
		After(limit.Truncate(time.Second))
	if modifiedAfter == failWhenNotModified {
		return 0, nil
	}
	return failureStatus, nil
}

func matchesETagHeader(value, current string, weak bool) (bool, error) {
	if strings.TrimSpace(value) == "*" {
		return true, nil
	}
	matched := false
	for _, candidate := range strings.Split(value, ",") {
		candidate = strings.TrimSpace(candidate)
		if strings.HasPrefix(candidate, "W/") {
			if !weak {
				continue
			}
			candidate = strings.TrimPrefix(candidate, "W/")
		}
		if len(candidate) < 2 || candidate[0] != '"' || candidate[len(candidate)-1] != '"' {
			return false, errInvalidEntityTag
		}
		expected := current
		if weak {
			expected = strings.TrimPrefix(expected, "W/")
		}
		if candidate == expected {
			matched = true
		}
	}
	return matched, nil
}
