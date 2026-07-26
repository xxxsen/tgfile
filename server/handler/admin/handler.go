package admin

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/xxxsen/common/logutil"
	"go.uber.org/zap"

	"github.com/xxxsen/tgfile/server/handler/admin/ui"
)

const (
	sessionCookieName = "tgfile_admin_session"
	principalKey      = "tgfile-admin-principal"
)

var errInvalidCredentialBounds = errors.New("invalid credential bounds")

func New(options Options) (*Handler, error) {
	origin, canonical, err := validateAdminHandlerOptions(options)
	if err != nil {
		return nil, err
	}
	applyAdminHandlerDefaults(&options)
	handler := &Handler{
		files:             options.FileManager,
		backups:           options.BackupManager,
		users:             cloneStringMap(options.Users),
		roles:             cloneStringMap(options.Roles),
		externalCanonical: canonical,
		secureCookie:      origin.Scheme == "https",
		maxUploadSize:     options.MaxUploadSize,
		maxPathBytes:      options.MaxPathBytes,
		mutationMaxItems:  options.MutationMaxItems,
		sessions:          newSessionStore(options.SessionIdle, options.SessionMaximum),
		loginLimiter:      newLoginLimiter(),
	}
	if _, err := io.ReadFull(rand.Reader, handler.dummyPassword[:]); err != nil {
		return nil, fmt.Errorf("generate admin dummy credential: %w", err)
	}
	handler.api = handler.newAPIHandler()
	return handler, nil
}

func validateAdminHandlerOptions(options Options) (*url.URL, string, error) {
	if options.FileManager == nil || options.BackupManager == nil ||
		len(options.Users) == 0 || len(options.Roles) == 0 ||
		options.SessionIdle <= 0 || options.SessionMaximum <= options.SessionIdle ||
		options.MaxUploadSize < 1 || options.MaxPathBytes < 0 ||
		options.MutationMaxItems < 0 {
		return nil, "", errInvalidAdminOptions
	}
	origin, canonical, err := parseOrigin(options.ExternalOrigin)
	if err != nil {
		return nil, "", err
	}
	if origin.Scheme == "http" && !isLoopbackHost(origin.Hostname()) {
		return nil, "", errInsecureAdminOrigin
	}
	if err := validateAdminHandlerUsers(options.Users, options.Roles); err != nil {
		return nil, "", err
	}
	return origin, canonical, nil
}

func validateAdminHandlerUsers(users, roles map[string]string) error {
	for username, role := range roles {
		password, exists := users[username]
		if !exists || password == "" || len(password) > 4096 ||
			username == "" || len(username) > 256 ||
			strings.IndexFunc(username, isControlCharacter) >= 0 ||
			(role != roleRead && role != roleReadWrite) {
			return errInvalidAdminUsers
		}
	}
	return nil
}

func applyAdminHandlerDefaults(options *Options) {
	if options.MaxPathBytes == 0 {
		options.MaxPathBytes = 1024
	}
	if options.MutationMaxItems == 0 {
		options.MutationMaxItems = 100_000
	}
}

func isLoopbackHost(hostname string) bool {
	if strings.EqualFold(hostname, "localhost") {
		return true
	}
	address := net.ParseIP(hostname)
	return address != nil && address.IsLoopback()
}

func (h *Handler) Register(router *gin.RouterGroup) {
	router.GET("/_admin", func(c *gin.Context) {
		c.Redirect(http.StatusPermanentRedirect, "/_admin/")
	})
	admin := router.Group("/_admin")
	admin.GET("/", h.index)
	admin.HEAD("/", h.index)
	admin.GET("/assets/app.js", h.asset("app.js", "text/javascript; charset=utf-8"))
	admin.HEAD("/assets/app.js", h.asset("app.js", "text/javascript; charset=utf-8"))
	admin.GET("/assets/styles.css", h.asset("styles.css", "text/css; charset=utf-8"))
	admin.HEAD("/assets/styles.css", h.asset("styles.css", "text/css; charset=utf-8"))

	admin.Any("/api/*all", gin.WrapH(h.api))
}

func (h *Handler) newAPIHandler() http.Handler {
	apiEngine := gin.New()
	apiEngine.RedirectTrailingSlash = false
	apiEngine.RedirectFixedPath = false
	apiEngine.Use(h.auditRequest)
	api := apiEngine.Group("/_admin/api/v1")
	api.POST("/session", h.login)
	authenticated := api.Group("")
	authenticated.Use(h.requireSession)
	authenticated.GET("/session", h.currentSession)
	authenticated.DELETE("/session", h.logout)
	authenticated.GET("/entries/stat", h.statEntry)
	authenticated.GET("/entries", h.listEntries)
	authenticated.GET("/content", h.download)
	authenticated.HEAD("/content", h.download)
	authenticated.PUT("/content", h.upload)
	authenticated.GET("/backup/jobs", h.listJobs)
	authenticated.GET("/backup/jobs/:job_id", h.getJob)
	authenticated.POST("/backup/jobs/:job_id/cancel", h.cancelJob)
	authenticated.POST("/backup/exports", h.createExport)
	authenticated.POST("/backup/imports", h.createImport)
	authenticated.GET("/backup/exports/:job_id/artifact", h.artifact)
	authenticated.HEAD("/backup/exports/:job_id/artifact", h.artifact)
	return apiEngine
}

func (h *Handler) index(c *gin.Context) {
	content, err := ui.Read("index.html")
	if err != nil {
		h.writeMappedError(c, err)
		return
	}
	setAdminSecurityHeaders(c.Writer.Header())
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.Header("Cache-Control", "no-store")
	if c.Request.Method == http.MethodHead {
		c.Status(http.StatusOK)
		return
	}
	c.Data(http.StatusOK, "text/html; charset=utf-8", content)
}

func (h *Handler) asset(name, contentType string) gin.HandlerFunc {
	return func(c *gin.Context) {
		content, err := ui.Read(name)
		if err != nil {
			h.writeMappedError(c, err)
			return
		}
		digest := sha256.Sum256(content)
		etag := `"` + hex.EncodeToString(digest[:]) + `"`
		setAdminSecurityHeaders(c.Writer.Header())
		c.Header("Content-Type", contentType)
		c.Header("Cache-Control", "no-cache")
		c.Header("ETag", etag)
		if c.GetHeader("If-None-Match") == etag {
			c.Status(http.StatusNotModified)
			return
		}
		if c.Request.Method == http.MethodHead {
			c.Status(http.StatusOK)
			return
		}
		c.Data(http.StatusOK, contentType, content)
	}
}

func setAdminSecurityHeaders(header http.Header) {
	header.Set(
		"Content-Security-Policy",
		"default-src 'none'; script-src 'self'; style-src 'self'; "+
			"img-src 'self' data:; connect-src 'self'; font-src 'self'; "+
			"base-uri 'none'; form-action 'self'; frame-ancestors 'none'; object-src 'none'",
	)
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (h *Handler) login(c *gin.Context) {
	started := time.Now()
	if _, ok := h.parseQuery(c); !ok {
		return
	}
	if !h.requireOrigin(c) {
		return
	}
	if c.ContentType() != "application/json" {
		h.writePublicError(c, http.StatusBadRequest, "invalid_request", "请求格式无效", nil)
		return
	}
	request, err := decodeLoginRequest(c.Request.Body)
	if err != nil {
		h.logLoginFailure(c, request.Username, "malformed_credentials")
		h.delayLoginFailure(c, started)
		h.writePublicError(
			c,
			http.StatusUnauthorized,
			"invalid_credentials",
			"用户名或密码错误",
			err,
		)
		return
	}
	key, allowed := h.loginLimiter.begin(directPeerIP(c.Request), request.Username, time.Now())
	if !allowed {
		h.logLoginFailure(c, request.Username, "rate_limited")
		h.writePublicError(
			c,
			http.StatusTooManyRequests,
			"login_rate_limited",
			"登录尝试过多，请稍后重试",
			nil,
		)
		return
	}
	role, valid := h.authenticateLogin(request)
	if !valid {
		h.loginLimiter.fail(key, time.Now())
		h.logLoginFailure(c, request.Username, "invalid_credentials")
		h.delayLoginFailure(c, started)
		h.writePublicError(
			c,
			http.StatusUnauthorized,
			"invalid_credentials",
			"用户名或密码错误",
			nil,
		)
		return
	}
	token, session, err := h.sessions.create(request.Username, role)
	if err != nil {
		h.writeMappedError(c, err)
		return
	}
	h.loginLimiter.success(key)
	c.Set(principalKey, principal{
		Username: session.username,
		Role:     session.role,
		CSRF:     session.csrf,
	})
	h.setSessionCookie(c, token, false)
	h.writeData(c, http.StatusOK, h.sessionDTO(session))
}

func decodeLoginRequest(body io.Reader) (loginRequest, error) {
	var request loginRequest
	if err := decodeStrictJSON(body, 16*1024, &request); err != nil {
		return request, fmt.Errorf("decode login request: %w", err)
	}
	if request.Username == "" || len(request.Username) > 256 ||
		request.Password == "" || len(request.Password) > 4096 {
		return request, errInvalidCredentialBounds
	}
	return request, nil
}

func (h *Handler) authenticateLogin(request loginRequest) (string, bool) {
	expected, userExists := h.users[request.Username]
	role, roleExists := h.roles[request.Username]
	expectedDigest := h.dummyPassword
	if userExists && roleExists {
		expectedDigest = sha256.Sum256([]byte(expected))
	}
	submittedDigest := sha256.Sum256([]byte(request.Password))
	valid := userExists && roleExists &&
		subtle.ConstantTimeCompare(submittedDigest[:], expectedDigest[:]) == 1
	return role, valid
}

func (h *Handler) logLoginFailure(c *gin.Context, username, reason string) {
	digest := sha256.Sum256([]byte(username))
	logutil.GetLogger(c.Request.Context()).Warn(
		"admin login rejected",
		zap.String("username_sha256_prefix", hex.EncodeToString(digest[:8])),
		zap.String("peer_ip", directPeerIP(c.Request)),
		zap.String("reason", reason),
	)
}

func (h *Handler) currentSession(c *gin.Context) {
	if _, ok := h.parseQuery(c); !ok {
		return
	}
	session, ok := h.sessionFromContext(c)
	if !ok {
		h.writePublicError(c, http.StatusUnauthorized, "unauthenticated", "请重新登录", nil)
		return
	}
	h.writeData(c, http.StatusOK, h.sessionDTO(session))
}

func (h *Handler) logout(c *gin.Context) {
	session, ok := h.principal(c)
	if !ok || !h.requireMutation(c, session) {
		return
	}
	if _, ok := h.parseQuery(c); !ok {
		return
	}
	token, ok := sessionCookie(c.Request)
	if ok {
		h.sessions.delete(token)
	}
	h.setSessionCookie(c, "", true)
	c.Header("Cache-Control", "no-store")
	c.Status(http.StatusNoContent)
}

func (h *Handler) requireSession(c *gin.Context) {
	token, ok := sessionCookie(c.Request)
	if !ok {
		h.writePublicError(c, http.StatusUnauthorized, "unauthenticated", "请重新登录", nil)
		c.Abort()
		return
	}
	session, exists := h.sessions.get(token)
	if !exists {
		h.setSessionCookie(c, "", true)
		h.writePublicError(c, http.StatusUnauthorized, "unauthenticated", "请重新登录", nil)
		c.Abort()
		return
	}
	c.Set(principalKey, principal{
		Username: session.username,
		Role:     session.role,
		CSRF:     session.csrf,
	})
	c.Set(principalKey+"-session", session)
	c.Header("Vary", "Cookie")
	c.Next()
}

func (h *Handler) principal(c *gin.Context) (principal, bool) {
	value, exists := c.Get(principalKey)
	if !exists {
		return principal{}, false
	}
	result, ok := value.(principal)
	return result, ok
}

func (h *Handler) sessionFromContext(c *gin.Context) (*sessionRecord, bool) {
	value, exists := c.Get(principalKey + "-session")
	if !exists {
		return nil, false
	}
	session, ok := value.(*sessionRecord)
	return session, ok
}

func (h *Handler) requireWrite(c *gin.Context) (principal, bool) {
	user, ok := h.principal(c)
	if !ok {
		h.writePublicError(c, http.StatusUnauthorized, "unauthenticated", "请重新登录", nil)
		return principal{}, false
	}
	if user.Role != roleReadWrite {
		h.writePublicError(c, http.StatusForbidden, "forbidden", "当前账号没有写权限", nil)
		return principal{}, false
	}
	return user, true
}

func (h *Handler) requireMutation(c *gin.Context, user principal) bool {
	if !h.requireOrigin(c) {
		return false
	}
	values := c.Request.Header.Values("X-Csrf-Token")
	if len(values) != 1 {
		h.writePublicError(c, http.StatusForbidden, "csrf_invalid", "安全令牌无效", nil)
		return false
	}
	expected := sha256.Sum256([]byte(user.CSRF))
	actual := sha256.Sum256([]byte(values[0]))
	if subtle.ConstantTimeCompare(expected[:], actual[:]) != 1 {
		h.writePublicError(c, http.StatusForbidden, "csrf_invalid", "安全令牌无效", nil)
		return false
	}
	return true
}

func (h *Handler) requireOrigin(c *gin.Context) bool {
	values := c.Request.Header.Values("Origin")
	if len(values) != 1 {
		h.writePublicError(c, http.StatusForbidden, "origin_invalid", "请求来源无效", nil)
		return false
	}
	_, canonical, err := parseOrigin(values[0])
	if err != nil || canonical != h.externalCanonical {
		h.writePublicError(c, http.StatusForbidden, "origin_invalid", "请求来源无效", err)
		return false
	}
	return true
}

func (h *Handler) setSessionCookie(c *gin.Context, value string, remove bool) {
	cookie := &http.Cookie{
		Name:     sessionCookieName,
		Value:    value,
		Path:     "/_admin/",
		HttpOnly: true,
		Secure:   h.secureCookie,
		SameSite: http.SameSiteStrictMode,
	}
	if remove {
		cookie.MaxAge = -1
		cookie.Expires = time.Unix(1, 0)
	}
	http.SetCookie(c.Writer, cookie)
}

func sessionCookie(request *http.Request) (string, bool) {
	var value string
	count := 0
	for _, cookie := range request.Cookies() {
		if cookie.Name == sessionCookieName {
			count++
			value = cookie.Value
		}
	}
	return value, count == 1 && value != ""
}

func (h *Handler) sessionDTO(session *sessionRecord) map[string]any {
	return map[string]any{
		"username":            session.username,
		"role":                session.role,
		"csrf_token":          session.csrf,
		"idle_expires_at":     h.sessions.idleExpiry(session).UnixMilli(),
		"absolute_expires_at": session.expiresAt.UnixMilli(),
	}
}

func (h *Handler) delayLoginFailure(c *gin.Context, started time.Time) {
	remaining := 250*time.Millisecond - time.Since(started)
	if remaining <= 0 {
		return
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-c.Request.Context().Done():
	case <-timer.C:
	}
}

func parseOrigin(value string) (*url.URL, string, error) {
	origin, err := url.Parse(strings.TrimSpace(value))
	if err != nil {
		return nil, "", errInvalidAdminOrigin
	}
	if !validOriginShape(origin) {
		return nil, "", errInvalidAdminOrigin
	}
	hostname := strings.ToLower(origin.Hostname())
	port := canonicalOriginPort(origin.Scheme, origin.Port())
	host := canonicalOriginHost(hostname, port)
	canonical := origin.Scheme + "://" + host
	origin, _ = url.Parse(canonical)
	return origin, canonical, nil
}

func validOriginShape(origin *url.URL) bool {
	if origin.Scheme != "http" && origin.Scheme != "https" {
		return false
	}
	if origin.Host == "" || origin.Hostname() == "" || origin.User != nil ||
		!validOriginPort(origin.Port()) {
		return false
	}
	if origin.Path != "" && origin.Path != "/" {
		return false
	}
	return origin.RawQuery == "" && origin.Fragment == ""
}

func validOriginPort(port string) bool {
	if port == "" {
		return true
	}
	value, err := strconv.Atoi(port)
	return err == nil && value >= 1 && value <= 65535
}

func canonicalOriginPort(scheme, port string) string {
	if scheme == "http" && port == "80" {
		return ""
	}
	if scheme == "https" && port == "443" {
		return ""
	}
	return port
}

func canonicalOriginHost(hostname, port string) string {
	if port != "" {
		return net.JoinHostPort(hostname, port)
	}
	if strings.Contains(hostname, ":") {
		return "[" + hostname + "]"
	}
	return hostname
}

func directPeerIP(request *http.Request) string {
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err == nil {
		return host
	}
	if len(request.RemoteAddr) > 256 {
		return request.RemoteAddr[:256]
	}
	return request.RemoteAddr
}

func cloneStringMap(input map[string]string) map[string]string {
	result := make(map[string]string, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}

func isControlCharacter(value rune) bool {
	return value < 0x20 || value == 0x7f
}

func safeDisposition(filename string) string {
	fallback := asciiFilenameFallback(filename)
	value := mime.FormatMediaType("attachment", map[string]string{"filename": fallback})
	if value == "" {
		return "attachment"
	}
	return value + "; filename*=UTF-8''" + url.PathEscape(filename)
}

func asciiFilenameFallback(filename string) string {
	var output strings.Builder
	for _, character := range filename {
		if character >= 0x20 && character <= 0x7e &&
			character != '"' && character != '\\' && character != '/' {
			output.WriteRune(character)
		} else {
			output.WriteByte('_')
		}
	}
	result := strings.TrimSpace(output.String())
	if result == "" || result == "." || result == ".." {
		return "download"
	}
	if len(result) > 150 {
		result = result[:150]
	}
	return result
}

func parsePositiveInt(value string, fallback, maximum int) (int, error) {
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 1 || parsed > maximum {
		return 0, errInvalidIntegerParam
	}
	return parsed, nil
}
