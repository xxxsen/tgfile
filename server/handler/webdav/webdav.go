package webdav

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/xxxsen/common/logutil"
	"github.com/xxxsen/common/webapi/proxyutil"
	"go.uber.org/zap"

	"github.com/xxxsen/tgfile/authz"
	"github.com/xxxsen/tgfile/directory"
	"github.com/xxxsen/tgfile/entity"
	"github.com/xxxsen/tgfile/filemgr"
)

const (
	davNamespace            = "DAV:"
	maxDAVXMLBodySize int64 = 1 << 20
)

var (
	errUnsupportedMethod       = errors.New("unsupported WebDAV method")
	errDestinationWebRoot      = errors.New("destination is outside WebDAV root")
	errDestinationOrigin       = errors.New("destination uses a different origin")
	errDirectoryStream         = errors.New("cannot open a stream on a directory")
	errMKCOLBody               = errors.New("MKCOL request body is not supported")
	errInvalidDepth            = errors.New("invalid WebDAV Depth header")
	errInfinitePropfind        = errors.New("infinite-depth PROPFIND is disabled")
	errInvalidOverwrite        = errors.New("invalid WebDAV Overwrite header")
	errSameResource            = errors.New("source and destination are the same resource")
	errInvalidDestination      = errors.New("invalid WebDAV Destination header")
	errInvalidEncodedSeparator = errors.New("encoded path separator is not allowed")
	errReadOnly                = errors.New("WebDAV account is read-only")
	errInvalidIfHeader         = errors.New("invalid WebDAV If header")
	errInvalidLockToken        = errors.New("invalid WebDAV Lock-Token header")
	errInvalidCondition        = errors.New("invalid HTTP conditional header")
	errInvalidEntityTag        = errors.New("invalid entity-tag list")
	errDAVXMLBodyTooLarge      = errors.New("WebDAV XML body exceeds the configured limit")
	errInvalidSyncToken        = errors.New("invalid WebDAV sync token")
	errUnsupportedLockTimeout  = errors.New("unsupported WebDAV lock timeout")
	errInvalidLockInfo         = errors.New("invalid DAV:lockinfo body")
	errUnsupportedLockScope    = errors.New("only exclusive write locks are supported")
	errInvalidPropfind         = errors.New("invalid DAV:propfind body")
	errInvalidPropfindElement  = errors.New("invalid PROPFIND instruction")
	errMultiplePropfindModes   = errors.New("multiple PROPFIND selection modes")
	errMissingPropfindMode     = errors.New("PROPFIND selection is missing")
	errInvalidPropfindInclude  = errors.New("DAV:include requires DAV:allprop")
	errPropPatchBodyRequired   = errors.New("PROPPATCH body is required")
	errInvalidPropertyUpdate   = errors.New("invalid DAV:propertyupdate body")
	errInvalidPropPatchOp      = errors.New("invalid PROPPATCH operation")
	errEmptyPropPatch          = errors.New("PROPPATCH operation has no properties")
	errSyncRootNotCollection   = errors.New("sync root is not a collection")
	errReportBodyRequired      = errors.New("REPORT body is required")
	errUnsupportedReport       = errors.New("unsupported REPORT body")
	errInvalidSyncInstruction  = errors.New("invalid sync-collection instruction")
	errUnsupportedSyncLevel    = errors.New("unsupported sync level")
	errPUTLengthUnknown        = errors.New("WebDAV PUT length is unknown")
	errPUTTooLarge             = errors.New("WebDAV PUT exceeds max_upload_size")
)

type Options struct {
	Authorizer         *authz.Authorizer
	ExternalOrigins    []string
	MaxUploadSize      int64
	QuotaBytes         int64
	MaxMutationEntries int
	SyncPageSize       int
}

type WebdavHandler struct {
	fmgr               filemgr.IFileManager
	davRoot            string
	webRoot            string
	authorizer         *authz.Authorizer
	externalOrigins    []*url.URL
	maxUploadSize      int64
	quotaBytes         int64
	maxMutationEntries int
	syncPageSize       int
}

func NewWebdavHandler(
	fmgr filemgr.IFileManager,
	davRoot, webRoot string,
	options ...Options,
) *WebdavHandler {
	if strings.TrimSpace(davRoot) == "" {
		davRoot = "/"
	}
	handler := &WebdavHandler{
		fmgr:         fmgr,
		davRoot:      path.Clean(davRoot),
		webRoot:      strings.TrimSuffix(webRoot, "/"),
		syncPageSize: 1000,
	}
	if len(options) != 0 {
		handler.authorizer = options[0].Authorizer
		for _, value := range options[0].ExternalOrigins {
			origin, err := url.Parse(value)
			if err != nil {
				panic(fmt.Errorf("parse WebDAV external origin %q: %w", value, err))
			}
			handler.externalOrigins = append(handler.externalOrigins, origin)
		}
		handler.maxUploadSize = options[0].MaxUploadSize
		handler.quotaBytes = options[0].QuotaBytes
		handler.maxMutationEntries = options[0].MaxMutationEntries
		if options[0].SyncPageSize > 0 {
			handler.syncPageSize = options[0].SyncPageSize
		}
	}
	if err := handler.initWebdav(handler.davRoot); err != nil {
		panic(err)
	}
	return handler
}

func (h *WebdavHandler) Handler(c *gin.Context) {
	if err := h.validateRequestPath(c.Request.URL); err != nil {
		h.writeError(c, http.StatusBadRequest, err, "")
		return
	}
	if !h.authorize(c) {
		return
	}
	handlers := map[string]func(*gin.Context){
		http.MethodGet:     h.handleGet,
		http.MethodPut:     h.handlePut,
		http.MethodDelete:  h.handleDelete,
		http.MethodHead:    h.handleHead,
		http.MethodOptions: h.handleOption,
		"PROPFIND":         h.handlePropfind,
		"PROPPATCH":        h.handlePropPatch,
		"COPY":             h.handleCopy,
		"MOVE":             h.handleMove,
		"MKCOL":            h.handleMkcol,
		"LOCK":             h.handleLock,
		"UNLOCK":           h.handleUnlock,
		"REPORT":           h.handleReport,
	}
	handler, supported := handlers[c.Request.Method]
	if supported {
		handler(c)
		return
	}
	if _, ok := h.stat(c); !ok {
		return
	}
	c.Header("Allow", strings.Join(h.allowedMethods(c), ", "))
	h.writeError(c, http.StatusMethodNotAllowed, errUnsupportedMethod, "")
}

func (h *WebdavHandler) authorize(c *gin.Context) bool {
	user, ok := proxyutil.GetUserInfo(c.Request.Context())
	if !ok {
		c.Header("WWW-Authenticate", `Basic realm="Restricted Area"`)
		c.AbortWithStatus(http.StatusUnauthorized)
		return false
	}
	level := h.authorizer.Level(user.Username, authz.WebDAVRead, authz.WebDAVWrite)
	if level == authz.LevelNone {
		h.writeError(c, http.StatusForbidden, errReadOnly, "")
		return false
	}
	if level == authz.LevelRead && isWebDAVWriteMethod(c.Request.Method) {
		c.Header("Allow", strings.Join(ReadOnlyMethods, ", "))
		h.writeError(c, http.StatusForbidden, errReadOnly, "need-privileges")
		return false
	}
	return true
}

func isWebDAVWriteMethod(method string) bool {
	switch method {
	case http.MethodPut, http.MethodDelete, "PROPPATCH", "COPY", "MOVE", "MKCOL", "LOCK", "UNLOCK":
		return true
	default:
		return false
	}
}

func (h *WebdavHandler) principal(c *gin.Context) string {
	user, ok := proxyutil.GetUserInfo(c.Request.Context())
	if !ok {
		return ""
	}
	return user.Username
}

func (h *WebdavHandler) allowedMethods(c *gin.Context) []string {
	user, ok := proxyutil.GetUserInfo(c.Request.Context())
	if ok &&
		h.authorizer.Level(user.Username, authz.WebDAVRead, authz.WebDAVWrite) == authz.LevelRead {
		return ReadOnlyMethods
	}
	return AllowMethods
}

func (h *WebdavHandler) mutationOptions(
	c *gin.Context,
	condition *filemgr.WebDAVCondition,
) filemgr.WebDAVMutationOptions {
	return filemgr.WebDAVMutationOptions{
		Principal:  h.principal(c),
		Condition:  condition,
		MaxEntries: h.maxMutationEntries,
		QuotaRoot:  h.davRoot,
		QuotaBytes: h.quotaBytes,
	}
}

func (h *WebdavHandler) buildSrcPath(c *gin.Context) string {
	externalPath := c.Request.URL.Path
	relative := strings.TrimPrefix(externalPath, h.webRoot)
	return h.internalPath(relative)
}

func (h *WebdavHandler) internalPath(relative string) string {
	relative = "/" + strings.TrimPrefix(relative, "/")
	if h.davRoot == "/" {
		return path.Clean(relative)
	}
	return path.Join(h.davRoot, path.Clean(relative))
}

func (h *WebdavHandler) externalPath(internal string, collection bool) string {
	internal = path.Clean(internal)
	relative := internal
	if h.davRoot != "/" {
		relative = strings.TrimPrefix(internal, h.davRoot)
	}
	external := path.Join(h.webRoot, relative)
	if external == "." {
		external = "/"
	}
	if collection && !strings.HasSuffix(external, "/") {
		external += "/"
	}
	return (&url.URL{Path: external}).EscapedPath()
}

func (h *WebdavHandler) tryBuildDstPath(c *gin.Context) (string, error) {
	value := strings.TrimSpace(c.GetHeader("Destination"))
	if value == "" {
		return "", errInvalidDestination
	}
	uri, err := url.Parse(value)
	if err != nil ||
		uri.User != nil ||
		uri.Opaque != "" ||
		(uri.Scheme == "") != (uri.Host == "") ||
		uri.RawQuery != "" ||
		uri.Fragment != "" {
		return "", fmt.Errorf("%w: %s", errInvalidDestination, value)
	}
	if err := h.validateRequestPath(uri); err != nil {
		return "", err
	}
	if uri.IsAbs() {
		if !h.absoluteOriginAllowed(uri, c.Request) {
			return "", errDestinationOrigin
		}
	}
	if uri.Path != h.webRoot && !strings.HasPrefix(uri.Path, h.webRoot+"/") {
		return "", fmt.Errorf("%w: %s", errDestinationWebRoot, value)
	}
	relative := strings.TrimPrefix(uri.Path, h.webRoot)
	destination := h.internalPath(relative)
	if !pathWithinRoot(h.davRoot, destination) {
		return "", errDestinationWebRoot
	}
	if destination == h.buildSrcPath(c) {
		return "", errSameResource
	}
	return destination, nil
}

func (h *WebdavHandler) absoluteOriginAllowed(
	destination *url.URL,
	request *http.Request,
) bool {
	if len(h.externalOrigins) == 0 {
		return sameOrigin(destination, requestOrigin(request))
	}
	for _, origin := range h.externalOrigins {
		if sameOrigin(destination, origin) {
			return true
		}
	}
	return false
}

func requestOrigin(request *http.Request) *url.URL {
	scheme := "http"
	if request.TLS != nil {
		scheme = "https"
	}
	return &url.URL{Scheme: scheme, Host: request.Host}
}

func sameOrigin(left, right *url.URL) bool {
	if left == nil || right == nil ||
		!strings.EqualFold(left.Scheme, right.Scheme) ||
		!strings.EqualFold(left.Hostname(), right.Hostname()) {
		return false
	}
	return effectiveOriginPort(left) == effectiveOriginPort(right)
}

func effectiveOriginPort(origin *url.URL) string {
	if port := origin.Port(); port != "" {
		return port
	}
	switch strings.ToLower(origin.Scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	default:
		return ""
	}
}

func (h *WebdavHandler) validateRequestPath(uri *url.URL) error {
	escaped := strings.ToLower(uri.EscapedPath())
	if strings.Contains(escaped, "%2f") ||
		strings.Contains(escaped, "%5c") ||
		strings.ContainsRune(uri.Path, '\x00') ||
		strings.ContainsRune(uri.Path, '\\') {
		return errInvalidEncodedSeparator
	}
	return nil
}

func pathWithinRoot(root, candidate string) bool {
	root = path.Clean(root)
	candidate = path.Clean(candidate)
	return root == "/" || candidate == root || strings.HasPrefix(candidate, root+"/")
}

func (h *WebdavHandler) initWebdav(root string) error {
	if err := h.fmgr.CreateFileLink(context.Background(), root, 0, 0, true); err != nil {
		return fmt.Errorf("create WebDAV root %q: %w", root, err)
	}
	return nil
}

func (h *WebdavHandler) stat(c *gin.Context) (*entity.FileLinkMeta, bool) {
	resourcePath := h.buildSrcPath(c)
	item, err := h.fmgr.StatFileLink(c.Request.Context(), resourcePath)
	if err == nil {
		return item, true
	}
	if errors.Is(err, os.ErrNotExist) {
		h.writeError(c, http.StatusNotFound, err, "")
		return nil, false
	}
	h.writeMappedError(c, fmt.Errorf("stat WebDAV resource: %w", err))
	return nil, false
}

func (h *WebdavHandler) writeMappedError(c *gin.Context, err error) {
	status := http.StatusInternalServerError
	precondition := ""
	switch {
	case errors.Is(err, os.ErrNotExist), errors.Is(err, directory.ErrSourceNotFound):
		status = http.StatusNotFound
	case errors.Is(err, os.ErrExist), errors.Is(err, directory.ErrEntryNotFile):
		status = http.StatusMethodNotAllowed
	case errors.Is(err, directory.ErrParentNotFound),
		errors.Is(err, directory.ErrPathComponentNotDirectory):
		status = http.StatusConflict
	case errors.Is(err, directory.ErrDestinationExists),
		errors.Is(err, filemgr.ErrWebDAVPrecondition):
		status = http.StatusPreconditionFailed
	case errors.Is(err, filemgr.ErrWebDAVLocked), errors.Is(err, filemgr.ErrWebDAVLockToken):
		status = http.StatusLocked
		precondition = "lock-token-submitted"
	case errors.Is(err, filemgr.ErrWebDAVQuota), errors.Is(err, filemgr.ErrWebDAVTooManyItems):
		status = http.StatusInsufficientStorage
	case errors.Is(err, filemgr.ErrWebDAVSyncToken):
		status = http.StatusForbidden
		precondition = "valid-sync-token"
	case errors.Is(err, directory.ErrDestinationInsideSource), errors.Is(err, errSameResource):
		status = http.StatusForbidden
	case errors.Is(err, errDestinationOrigin):
		status = http.StatusBadGateway
	case errors.Is(err, errInvalidDestination),
		errors.Is(err, errDestinationWebRoot),
		errors.Is(err, errInvalidEncodedSeparator),
		errors.Is(err, errInvalidDepth),
		errors.Is(err, errInvalidOverwrite),
		errors.Is(err, errInvalidIfHeader),
		errors.Is(err, errInvalidCondition):
		status = http.StatusBadRequest
	}
	h.writeError(c, status, err, precondition)
}

func (h *WebdavHandler) writeError(
	c *gin.Context,
	status int,
	err error,
	precondition string,
) {
	setPrivateDAVHeaders(c.Writer.Header())
	if status == http.StatusMethodNotAllowed {
		c.Header("Allow", strings.Join(h.allowedMethods(c), ", "))
	}
	if precondition == "" {
		c.Status(status)
		return
	}
	c.Header("Content-Type", "application/xml; charset=utf-8")
	c.Status(status)
	encoder := xml.NewEncoder(c.Writer)
	start := xml.StartElement{
		Name: xml.Name{Space: davNamespace, Local: "error"},
	}
	if encodeErr := encoder.EncodeToken(start); encodeErr == nil {
		_ = encoder.EncodeToken(xml.StartElement{
			Name: xml.Name{Space: davNamespace, Local: precondition},
		})
		_ = encoder.EncodeToken(xml.EndElement{
			Name: xml.Name{Space: davNamespace, Local: precondition},
		})
		_ = encoder.EncodeToken(start.End())
		_ = encoder.Flush()
	}
	logutil.GetLogger(c.Request.Context()).Debug(
		"WebDAV request rejected",
		zap.Int("status", status),
		zap.Error(err),
	)
}

func setPrivateDAVHeaders(header http.Header) {
	header.Set("Cache-Control", "private, no-cache")
	header.Set("Vary", "Authorization")
}

func parseOverwrite(value string) (bool, error) {
	switch value {
	case "", "T":
		return true, nil
	case "F":
		return false, nil
	default:
		return false, errInvalidOverwrite
	}
}

func parseHTTPDateHeader(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	parsed, err := http.ParseTime(value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse HTTP date %q: %w", value, err)
	}
	return parsed, nil
}

func (h *WebdavHandler) requestCondition(
	c *gin.Context,
) (*filemgr.WebDAVCondition, error) {
	if err := validateEntityTagHeader(c.GetHeader("If-Match")); err != nil {
		return nil, fmt.Errorf("%w: If-Match: %w", errInvalidCondition, err)
	}
	if err := validateEntityTagHeader(c.GetHeader("If-None-Match")); err != nil {
		return nil, fmt.Errorf("%w: If-None-Match: %w", errInvalidCondition, err)
	}
	modifiedValue, err := parseHTTPDateHeader(c.GetHeader("If-Modified-Since"))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errInvalidCondition, err)
	}
	unmodifiedValue, err := parseHTTPDateHeader(c.GetHeader("If-Unmodified-Since"))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errInvalidCondition, err)
	}
	var modified, unmodified *time.Time
	if !modifiedValue.IsZero() {
		modified = &modifiedValue
	}
	if !unmodifiedValue.IsZero() {
		unmodified = &unmodifiedValue
	}
	ifHeader, err := h.parseIfHeader(c, c.GetHeader("If"))
	if err != nil {
		return nil, err
	}
	return &filemgr.WebDAVCondition{
		IfMatch:           c.GetHeader("If-Match"),
		IfNoneMatch:       c.GetHeader("If-None-Match"),
		IfModifiedSince:   modified,
		IfUnmodifiedSince: unmodified,
		IfHeader:          ifHeader,
		RequestPath:       h.buildSrcPath(c),
	}, nil
}

func validateEntityTagHeader(value string) error {
	if value == "" || strings.TrimSpace(value) == "*" {
		return nil
	}
	for _, candidate := range strings.Split(value, ",") {
		candidate = strings.TrimSpace(candidate)
		candidate = strings.TrimPrefix(candidate, "W/")
		if len(candidate) < 2 ||
			candidate[0] != '"' ||
			candidate[len(candidate)-1] != '"' ||
			strings.ContainsAny(candidate[1:len(candidate)-1], "\"\r\n") {
			return errInvalidEntityTag
		}
	}
	return nil
}

func (h *WebdavHandler) parseIfHeader(
	c *gin.Context,
	value string,
) (*filemgr.WebDAVIfHeader, error) {
	if strings.TrimSpace(value) == "" {
		return &filemgr.WebDAVIfHeader{}, nil
	}
	parser := davIfParser{value: value}
	header, err := parser.parse()
	if err != nil {
		return nil, err
	}
	for index := range header.Lists {
		if header.Lists[index].Resource == "" {
			continue
		}
		resource, err := h.ifResourcePath(c, header.Lists[index].Resource)
		if err != nil {
			return nil, err
		}
		header.Lists[index].Resource = resource
	}
	return header, nil
}

func (h *WebdavHandler) ifResourcePath(c *gin.Context, value string) (string, error) {
	uri, err := url.Parse(value)
	if err != nil ||
		uri.User != nil ||
		uri.Opaque != "" ||
		(uri.Scheme == "") != (uri.Host == "") ||
		uri.RawQuery != "" ||
		uri.Fragment != "" {
		return "", errInvalidIfHeader
	}
	if err := h.validateRequestPath(uri); err != nil {
		return "", err
	}
	if uri.IsAbs() {
		if !h.absoluteOriginAllowed(uri, c.Request) {
			return "", errInvalidIfHeader
		}
	}
	if uri.Path != h.webRoot && !strings.HasPrefix(uri.Path, h.webRoot+"/") {
		return "", errInvalidIfHeader
	}
	return h.internalPath(strings.TrimPrefix(uri.Path, h.webRoot)), nil
}

type davIfParser struct {
	value string
	index int
}

func (p *davIfParser) parse() (*filemgr.WebDAVIfHeader, error) {
	result := &filemgr.WebDAVIfHeader{}
	taggedResource := ""
	taggedMode := false
	tagHasList := false
	started := false
	for {
		p.skipSpace()
		if p.index == len(p.value) {
			break
		}
		switch p.value[p.index] {
		case '<':
			if started && !taggedMode {
				return nil, errInvalidIfHeader
			}
			if taggedMode && !tagHasList {
				return nil, errInvalidIfHeader
			}
			value, err := p.readDelimited('<', '>')
			if err != nil {
				return nil, err
			}
			taggedResource = value
			taggedMode = true
			tagHasList = false
			started = true
		case '(':
			terms, err := p.readList()
			if err != nil {
				return nil, err
			}
			result.Lists = append(result.Lists, filemgr.WebDAVIfList{
				Resource: taggedResource,
				Terms:    terms,
			})
			tagHasList = true
			started = true
		default:
			return nil, errInvalidIfHeader
		}
	}
	if len(result.Lists) == 0 || (taggedMode && !tagHasList) {
		return nil, errInvalidIfHeader
	}
	return result, nil
}

func (p *davIfParser) readList() ([]filemgr.WebDAVIfTerm, error) {
	p.index++
	terms := make([]filemgr.WebDAVIfTerm, 0, 2)
	for {
		p.skipSpace()
		if p.index >= len(p.value) {
			return nil, errInvalidIfHeader
		}
		if p.value[p.index] == ')' {
			p.index++
			if len(terms) == 0 {
				return nil, errInvalidIfHeader
			}
			return terms, nil
		}
		not := false
		if strings.HasPrefix(p.value[p.index:], "Not") &&
			(p.index+3 == len(p.value) || isDAVSpace(p.value[p.index+3])) {
			not = true
			p.index += 3
			p.skipSpace()
		}
		if p.index >= len(p.value) {
			return nil, errInvalidIfHeader
		}
		var term filemgr.WebDAVIfTerm
		term.Not = not
		switch p.value[p.index] {
		case '<':
			token, err := p.readDelimited('<', '>')
			if err != nil || token == "" {
				return nil, errInvalidIfHeader
			}
			term.LockToken = token
		case '[':
			etag, err := p.readDelimited('[', ']')
			if err != nil || etag == "" {
				return nil, errInvalidIfHeader
			}
			term.ETag = etag
		default:
			return nil, errInvalidIfHeader
		}
		terms = append(terms, term)
	}
}

func (p *davIfParser) readDelimited(open, closing byte) (string, error) {
	if p.index >= len(p.value) || p.value[p.index] != open {
		return "", errInvalidIfHeader
	}
	start := p.index + 1
	end := strings.IndexByte(p.value[start:], closing)
	if end < 0 {
		return "", errInvalidIfHeader
	}
	end += start
	p.index = end + 1
	return p.value[start:end], nil
}

func (p *davIfParser) skipSpace() {
	for p.index < len(p.value) && isDAVSpace(p.value[p.index]) {
		p.index++
	}
}

func isDAVSpace(value byte) bool {
	return value == ' ' || value == '\t' || value == '\r' || value == '\n'
}

func readLimitedXMLBody(request *http.Request) ([]byte, error) {
	if request.Body == nil {
		return nil, nil
	}
	raw, err := io.ReadAll(io.LimitReader(request.Body, maxDAVXMLBodySize+1))
	if err != nil {
		return nil, fmt.Errorf("read WebDAV XML body: %w", err)
	}
	if int64(len(raw)) > maxDAVXMLBodySize {
		return nil, fmt.Errorf("%w: %d bytes", errDAVXMLBodyTooLarge, maxDAVXMLBodySize)
	}
	return raw, nil
}

func logCloseError(ctx context.Context, closer io.Closer, message string) {
	if err := closer.Close(); err != nil {
		logutil.GetLogger(ctx).Error(message, zap.Error(err))
	}
}

func parseSyncToken(value string) (int64, error) {
	const prefix = "urn:tgfile:webdav-sync:"
	if value == "" {
		return 0, nil
	}
	if !strings.HasPrefix(value, prefix) {
		return 0, errInvalidSyncToken
	}
	revision, err := strconv.ParseInt(strings.TrimPrefix(value, prefix), 10, 64)
	if err != nil || revision < 0 {
		return 0, errInvalidSyncToken
	}
	return revision, nil
}

func formatSyncToken(revision int64) string {
	return fmt.Sprintf("urn:tgfile:webdav-sync:%d", revision)
}
