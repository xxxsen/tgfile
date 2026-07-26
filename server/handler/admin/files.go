package admin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/xxxsen/common/logutil"
	"go.uber.org/zap"

	"github.com/xxxsen/tgfile/entity"
	"github.com/xxxsen/tgfile/filemgr"
	"github.com/xxxsen/tgfile/server/httpkit"
)

var strongAdminETag = regexp.MustCompile(`^"[0-9]+-[0-9]+"$`)

type entryCursor struct {
	Version       int    `json:"v"`
	ParentEntryID uint64 `json:"parent_entry_id"`
	FileKind      int    `json:"file_kind"`
	FileName      string `json:"file_name"`
	EntryID       uint64 `json:"entry_id"`
}

func (h *Handler) statEntry(c *gin.Context) {
	query, ok := h.parseQuery(c, "path")
	if !ok {
		return
	}
	resourcePath, ok := h.parsePath(c, query.Get("path"))
	if !ok {
		return
	}
	setAuditPath(c, resourcePath)
	entry, err := h.files.StatFileLink(c.Request.Context(), resourcePath)
	if err != nil {
		h.writeMappedError(c, err)
		return
	}
	h.writeData(c, http.StatusOK, h.entry(resourcePath, entry))
}

func (h *Handler) listEntries(c *gin.Context) {
	query, ok := h.parseQuery(c, "path", "limit", "cursor")
	if !ok {
		return
	}
	resourcePath, ok := h.parsePath(c, query.Get("path"))
	if !ok {
		return
	}
	setAuditPath(c, resourcePath)
	limit, err := parsePositiveInt(query.Get("limit"), 100, 500)
	if err != nil {
		h.writePublicError(c, http.StatusBadRequest, "invalid_request", "分页大小无效", err)
		return
	}
	decodedCursor, hasCursor, err := decodeEntryCursor(query.Get("cursor"))
	if err != nil {
		h.writePublicError(c, http.StatusBadRequest, "invalid_cursor", "分页游标无效", err)
		return
	}
	var cursor *filemgr.FileLinkPageCursor
	if hasCursor {
		cursor = &decodedCursor
	}
	result, err := h.files.ListFileLinksPage(c.Request.Context(), filemgr.FileLinkPageRequest{
		Path:   resourcePath,
		Cursor: cursor,
		Limit:  limit,
	})
	if err != nil {
		h.writeMappedError(c, err)
		return
	}
	items := make([]entryDTO, 0, len(result.Items))
	for _, item := range result.Items {
		items = append(items, h.entry(joinEntryPath(resourcePath, item.FileName), item))
	}
	nextCursor, err := encodeEntryCursor(result.NextCursor)
	if err != nil {
		h.writeMappedError(c, err)
		return
	}
	h.writeData(c, http.StatusOK, map[string]any{
		"path":        resourcePath,
		"items":       items,
		"next_cursor": nextCursor,
	})
}

func (h *Handler) download(c *gin.Context) {
	query, ok := h.parseQuery(c, "path")
	if !ok {
		return
	}
	resourcePath, ok := h.parsePath(c, query.Get("path"))
	if !ok {
		return
	}
	setAuditPath(c, resourcePath)
	info, err := h.files.StatFileLink(c.Request.Context(), resourcePath)
	if err != nil {
		h.writeMappedError(c, err)
		return
	}
	if info.IsDir {
		h.writeMappedError(c, filemgr.ErrDirectoryIO)
		return
	}
	stream, err := h.files.OpenFile(c.Request.Context(), info.FileId)
	if err != nil {
		h.writeMappedError(c, err)
		return
	}
	defer func() {
		if err := stream.Close(); err != nil {
			logutil.GetLogger(c.Request.Context()).Warn(
				"close admin download",
				zap.String("error_type", fmt.Sprintf("%T", err)),
			)
		}
	}()
	setAdminSecurityHeaders(c.Writer.Header())
	c.Header("Content-Type", httpkit.DetermineMimeType(info.FileName))
	c.Header("Content-Disposition", safeDisposition(info.FileName))
	c.Header("Cache-Control", "private, no-store")
	c.Header("ETag", filemgr.WebDAVETag(info))
	c.Header("Last-Modified", time.UnixMilli(info.Mtime).UTC().Format(http.TimeFormat))
	http.ServeContent(
		c.Writer,
		c.Request,
		info.FileName,
		time.UnixMilli(info.Mtime).UTC(),
		stream,
	)
}

func (h *Handler) upload(c *gin.Context) {
	user, ok := h.requireWrite(c)
	if !ok || !h.requireMutation(c, user) {
		return
	}
	query, ok := h.parseQuery(c, "path")
	if !ok {
		return
	}
	resourcePath, ok := h.parsePath(c, query.Get("path"))
	if !ok || resourcePath == "/" {
		if ok {
			h.writePublicError(c, http.StatusBadRequest, "invalid_path", "不能覆盖根目录", nil)
		}
		return
	}
	setAuditPath(c, resourcePath)
	contentLength := requestContentLength(c.Request)
	if contentLength < 0 {
		h.writePublicError(c, http.StatusLengthRequired, "length_required", "必须提供文件大小", nil)
		return
	}
	if contentLength > h.maxUploadSize {
		h.writePublicError(c, http.StatusRequestEntityTooLarge, "payload_too_large", "文件超过上传限制", nil)
		return
	}
	condition, ok := h.uploadCondition(c, resourcePath)
	if !ok {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, h.maxUploadSize+1)
	ctx := c.Request.Context()
	fileID, err := h.files.CreateFile(ctx, contentLength, c.Request.Body)
	if err != nil {
		h.writeMappedError(c, err)
		return
	}
	result, publishErr := h.files.PublishWebDAVFile(
		ctx,
		resourcePath,
		fileID,
		contentLength,
		filemgr.WebDAVMutationOptions{
			Principal:  user.Username,
			Condition:  condition,
			MaxEntries: h.mutationMaxItems,
		},
	)
	if publishErr != nil {
		cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		cleanupErr := h.files.DiscardUnpublishedFile(cleanupContext, fileID)
		cancel()
		h.writeMappedError(c, errors.Join(publishErr, cleanupErr))
		return
	}
	status := http.StatusOK
	if result.Created {
		status = http.StatusCreated
	}
	h.writeData(c, status, h.entry(resourcePath, result.Link))
}

func (h *Handler) uploadCondition(
	c *gin.Context,
	resourcePath string,
) (*filemgr.WebDAVCondition, bool) {
	ifMatch := c.Request.Header.Values("If-Match")
	ifNoneMatch := c.Request.Header.Values("If-None-Match")
	createOnly := len(ifNoneMatch) == 1 && ifNoneMatch[0] == "*" && len(ifMatch) == 0
	replace := len(ifMatch) == 1 && strongAdminETag.MatchString(ifMatch[0]) &&
		len(ifNoneMatch) == 0
	if !createOnly && !replace {
		h.writePublicError(
			c,
			http.StatusPreconditionRequired,
			"precondition_required",
			"上传必须指定创建或覆盖条件",
			nil,
		)
		return nil, false
	}
	condition := &filemgr.WebDAVCondition{RequestPath: resourcePath}
	if createOnly {
		condition.IfNoneMatch = "*"
	} else {
		condition.IfMatch = ifMatch[0]
	}
	return condition, true
}

func (h *Handler) entry(resourcePath string, item *entity.FileLinkMeta) entryDTO {
	kind := "file"
	etag := filemgr.WebDAVETag(item)
	if item.IsDir {
		kind = "directory"
		etag = ""
	}
	return entryDTO{
		Name:  item.FileName,
		Path:  resourcePath,
		Kind:  kind,
		Size:  item.FileSize,
		Ctime: item.Ctime,
		Mtime: item.Mtime,
		ETag:  etag,
	}
}

func joinEntryPath(parent, name string) string {
	if parent == "/" {
		return path.Join("/", name)
	}
	return path.Join(parent, name)
}

func (h *Handler) parseQuery(c *gin.Context, allowed ...string) (url.Values, bool) {
	values, err := url.ParseQuery(c.Request.URL.RawQuery)
	if err != nil {
		h.writePublicError(c, http.StatusBadRequest, "invalid_request", "查询参数无效", err)
		return nil, false
	}
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		allowedSet[name] = struct{}{}
	}
	for name, items := range values {
		if _, exists := allowedSet[name]; !exists || len(items) != 1 {
			h.writePublicError(c, http.StatusBadRequest, "invalid_request", "查询参数无效", nil)
			return nil, false
		}
	}
	return values, true
}

func (h *Handler) parsePath(c *gin.Context, value string) (string, bool) {
	if value == "" || len(value) > h.maxPathBytes || !utf8.ValidString(value) ||
		!strings.HasPrefix(value, "/") || path.Clean(value) != value ||
		strings.Contains(value, "\\") ||
		value != "/" && strings.HasSuffix(value, "/") {
		h.writePublicError(c, http.StatusBadRequest, "invalid_path", "路径无效", nil)
		return "", false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			h.writePublicError(c, http.StatusBadRequest, "invalid_path", "路径无效", nil)
			return "", false
		}
	}
	return value, true
}

func decodeEntryCursor(value string) (filemgr.FileLinkPageCursor, bool, error) {
	if value == "" {
		return filemgr.FileLinkPageCursor{}, false, nil
	}
	if len(value) > 2048 {
		return filemgr.FileLinkPageCursor{}, false, fmt.Errorf(
			"%w: too large",
			errInvalidEntryCursor,
		)
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return filemgr.FileLinkPageCursor{}, false, fmt.Errorf("decode entry cursor: %w", err)
	}
	var cursor entryCursor
	if err := decodeStrictJSON(strings.NewReader(string(raw)), 2048, &cursor); err != nil {
		return filemgr.FileLinkPageCursor{}, false, err
	}
	if cursor.Version != 1 || cursor.ParentEntryID == 0 ||
		(cursor.FileKind != 1 && cursor.FileKind != 2) ||
		cursor.FileName == "" || cursor.EntryID == 0 {
		return filemgr.FileLinkPageCursor{}, false, fmt.Errorf(
			"%w: invalid fields",
			errInvalidEntryCursor,
		)
	}
	return filemgr.FileLinkPageCursor{
		ParentEntryID: cursor.ParentEntryID,
		IsDir:         cursor.FileKind == 1,
		Name:          cursor.FileName,
		EntryID:       cursor.EntryID,
	}, true, nil
}

func encodeEntryCursor(cursor *filemgr.FileLinkPageCursor) (string, error) {
	if cursor == nil {
		return "", nil
	}
	kind := 2
	if cursor.IsDir {
		kind = 1
	}
	raw, err := jsonMarshal(entryCursor{
		Version:       1,
		ParentEntryID: cursor.ParentEntryID,
		FileKind:      kind,
		FileName:      cursor.Name,
		EntryID:       cursor.EntryID,
	})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func jsonMarshal(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode admin cursor: %w", err)
	}
	return raw, nil
}
