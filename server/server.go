package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/xxxsen/common/webapi"
	"github.com/xxxsen/common/webapi/auth"
	"github.com/xxxsen/common/webapi/middleware"
	"github.com/xxxsen/common/webapi/proxyutil"

	"github.com/xxxsen/tgfile/filemgr"
	"github.com/xxxsen/tgfile/server/handler/admin"
	"github.com/xxxsen/tgfile/server/handler/backup"
	"github.com/xxxsen/tgfile/server/handler/file"
	"github.com/xxxsen/tgfile/server/handler/s3"
	"github.com/xxxsen/tgfile/server/handler/s3/s3base"
	"github.com/xxxsen/tgfile/server/handler/webdav"
	"github.com/xxxsen/tgfile/server/model"

	"github.com/gin-gonic/gin"
)

func init() {
	gin.SetMode(gin.ReleaseMode)
}

var errAdminBackupManagerRequired = errors.New("admin requires a backup manager")

type Server struct {
	c             *config
	engine        webapi.IWebEngine
	bind          string
	s3            *s3.S3Handler
	webdavHandler *webdav.WebdavHandler
	adminHandler  *admin.Handler
}

func New(bind string, opts ...Option) (*Server, error) {
	c := applyOpts(opts...)
	svr := &Server{c: c, bind: bind}
	var err error
	if c.webdav.Enabled {
		if err := cleanupStaleWebDAVUploads(c.webdav.UploadTempDir, 24*time.Hour); err != nil {
			return nil, err
		}
	}
	if c.s3.Enabled {
		buckets := make([]s3.Bucket, 0, len(c.s3.Buckets))
		for _, bucket := range c.s3.Buckets {
			buckets = append(buckets, s3.Bucket{
				Name: bucket.Name,
				ACL:  s3.BucketACL(bucket.ACL),
			})
		}
		svr.s3 = s3.NewS3Handler(c.fmgr, s3.Config{
			Buckets:              buckets,
			MaxObjectSize:        c.s3.MaxObjectSize,
			MultipartExpireHours: c.s3.MultipartExpireHours,
			Users:                c.userMap,
		})
	}
	if c.admin.Enabled {
		if c.backupManager == nil {
			return nil, errAdminBackupManagerRequired
		}
		svr.adminHandler, err = admin.New(admin.Options{
			FileManager:      c.fmgr,
			BackupManager:    c.backupManager,
			Users:            c.userMap,
			Roles:            c.admin.Users,
			ExternalOrigins:  c.admin.ExternalOrigins,
			SessionIdle:      c.admin.SessionIdle,
			SessionMaximum:   c.admin.SessionMaximum,
			MaxUploadSize:    c.admin.MaxUploadSize,
			MaxPathBytes:     c.admin.MaxPathBytes,
			MutationMaxItems: c.admin.MaxMutationEntries,
		})
		if err != nil {
			return nil, fmt.Errorf("initialize admin handler: %w", err)
		}
	}
	svr.engine, err = webapi.NewEngine(
		"/",
		bind,
		webapi.WithAuth(auth.MapUserMatch(c.userMap)),
		webapi.WithAuthenticators(auth.NewBasic()),
		webapi.WithExtraMiddlewares(restoreOriginalRequestPath),
		webapi.WithRegister(svr.initAPI),
		webapi.WithNoRoute(svr.noRoute),
	)
	if err != nil {
		return nil, fmt.Errorf("create web engine: %w", err)
	}
	return svr, nil
}

func (s *Server) initAPI(router *gin.RouterGroup) {
	mustAuthMiddleware := middleware.MustAuthMiddleware()

	if s.adminHandler != nil {
		s.adminHandler.Register(router)
	}

	// handler here
	fileHandler := file.NewFileHandler(s.c.fmgr)

	fileRouter := router.Group("/file")
	{
		upload := proxyutil.WrapBizFunc(
			func(c *gin.Context, ctx context.Context, request any) {
				fileHandler.FileUpload(ctx, c, request)
			},
			&model.UploadFileRequest{},
		)
		fileRouter.POST("/upload", mustAuthMiddleware, upload)
		fileRouter.GET("/download/:key", fileHandler.FileDownload)
		fileRouter.GET("/meta/:key", fileHandler.GetMetaInfo)
		fileRouter.POST("/purge", mustAuthMiddleware, fileHandler.FilePurge)
	}
	staticRouter := router.Group("/static", mustAuthMiddleware)
	{
		staticRouter.StaticFS("", http.FS(filemgr.ToFileSystem(context.Background(), s.c.fmgr)))
	}

	if s.c.backup.Enabled && s.c.backupManager != nil {
		backupHandler := backup.New(s.c.backupManager, s.c.backup.Users)
		backupRouter := router.Group("/backup/v2", mustAuthMiddleware)
		backupRouter.POST("/exports", backupHandler.CreateExport)
		backupRouter.POST("/imports", backupHandler.CreateImport)
		backupRouter.GET("/jobs/:job_id", backupHandler.GetJob)
		backupRouter.POST("/jobs/:job_id/cancel", backupHandler.Cancel)
		backupRouter.GET("/exports/:job_id/artifact", backupHandler.Artifact)
		backupRouter.HEAD("/exports/:job_id/artifact", backupHandler.Artifact)
		backupRouter.GET("/metrics", backupHandler.Metrics)
	}
	if s.c.s3.Enabled {
		router.GET("", s.s3.RequestID, s.s3.ListBuckets)
		for _, bucket := range s.c.s3.Buckets {
			bucketRouter := router.Group(fmt.Sprintf("/%s", bucket.Name))
			bucketRouter.Use(s.s3.RequestID)
			bucketRouter.GET("", s.s3.GetBucket)
			bucketRouter.HEAD("", s.s3.HeadBucket)
			bucketRouter.PUT("", s.s3.NotImplemented)
			bucketRouter.DELETE("", s.s3.NotImplemented)
			bucketRouter.POST("", s.s3.PostBucketOrObject)
			bucketRouter.GET("/*object", s.s3.GetBucketOrObject)
			bucketRouter.HEAD("/*object", s.s3.HeadBucketOrObject)
			bucketRouter.POST("/*object", s.s3.PostBucketOrObject)
			bucketRouter.PUT("/*object", s.s3.UploadObject)
			bucketRouter.DELETE("/*object", s.s3.DeleteObject)
		}
	}
	if s.c.webdav.Enabled {
		webdavRouter := router.Group("/webdav", mustAuthMiddleware)
		{
			s.webdavHandler = webdav.NewWebdavHandler(
				s.c.fmgr,
				s.c.webdav.Root,
				webdavRouter.BasePath(),
				webdav.Options{
					Users:              s.c.webdav.Users,
					ExternalOrigins:    s.c.webdav.ExternalOrigins,
					MaxUploadSize:      s.c.webdav.MaxUploadSize,
					QuotaBytes:         s.c.webdav.QuotaBytes,
					MaxMutationEntries: s.c.webdav.MaxMutationEntries,
					SyncPageSize:       s.c.webdav.SyncPageSize,
				},
			)
			for _, method := range webdav.AllowMethods {
				webdavRouter.Handle(method, "/*all", s.webdavHandler.Handler)
			}
		}
	}
}

func (s *Server) noRoute(c *gin.Context) {
	if s.webdavHandler != nil && isWebDAVRequestPath(c.Request.URL.Path) {
		s.webdavHandler.Handler(c)
		return
	}
	if s.s3 == nil {
		c.Status(http.StatusNotFound)
		return
	}
	first, _, _ := strings.Cut(strings.TrimPrefix(c.Request.URL.Path, "/"), "/")
	switch first {
	case "", "_admin", "file", "static", "backup", "webdav":
		c.Status(http.StatusNotFound)
		return
	}
	if _, exists := s.s3.Bucket(first); exists {
		s.s3.NotImplemented(c)
		return
	}
	if _, apiError := s.s3.Authorize(c, true); apiError != nil {
		s3base.WriteError(c, apiError)
		return
	}
	apiError := s3base.NewError(
		http.StatusNotFound,
		"NoSuchBucket",
		"The specified bucket does not exist.",
		nil,
	)
	apiError.Bucket = first
	s3base.WriteError(c, apiError)
}

func (s *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if s.c != nil && s.c.webdav.Enabled && isWebDAVRequestPath(request.URL.Path) {
		writer.Header().Set("Cache-Control", "private, no-cache")
		writer.Header().Set("Vary", "Authorization")
	}
	if s.adminHandler != nil {
		request = admin.PreserveUnknownContentLength(request)
	}
	prepared, closePrepared, ok := s.prepareWebDAVPut(writer, request)
	if !ok {
		return
	}
	if closePrepared {
		defer func() {
			_ = prepared.Body.Close()
		}()
	}
	s.engine.ServeHTTP(writer, s.requestWithRedactedLogPath(prepared))
}

func (s *Server) prepareWebDAVPut(
	writer http.ResponseWriter,
	request *http.Request,
) (*http.Request, bool, bool) {
	if !s.shouldPrepareWebDAVPut(request) {
		return request, false, true
	}
	if status := s.webDAVPutAuthorizationStatus(request); status != 0 {
		if status == http.StatusUnauthorized {
			writer.Header().Set("WWW-Authenticate", `Basic realm="Restricted Area"`)
		}
		writer.WriteHeader(status)
		return request, false, false
	}
	if s.c.webdav.MaxUploadSize > 0 &&
		request.ContentLength > s.c.webdav.MaxUploadSize {
		writeWebDAVUploadError(writer, http.StatusRequestEntityTooLarge)
		return request, false, false
	}
	if request.ContentLength >= 0 {
		return request, false, true
	}
	prepared, err := s.spoolWebDAVPut(request)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errWebDAVUploadTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		writeWebDAVUploadError(writer, status)
		return request, false, false
	}
	return prepared, true, true
}

var errWebDAVUploadTooLarge = errors.New("WebDAV upload exceeds configured limit")

func isWebDAVRequestPath(requestPath string) bool {
	return requestPath == "/webdav" || strings.HasPrefix(requestPath, "/webdav/")
}

func (s *Server) shouldPrepareWebDAVPut(request *http.Request) bool {
	return s.c != nil &&
		s.c.webdav.Enabled &&
		request.Method == http.MethodPut &&
		isWebDAVRequestPath(request.URL.Path)
}

func (s *Server) webDAVPutAuthorizationStatus(request *http.Request) int {
	if len(request.Header.Values("Authorization")) != 1 {
		return http.StatusUnauthorized
	}
	username, password, ok := request.BasicAuth()
	if !ok {
		return http.StatusUnauthorized
	}
	expected, exists := s.c.userMap[username]
	if !exists ||
		subtle.ConstantTimeCompare([]byte(password), []byte(expected)) != 1 {
		return http.StatusUnauthorized
	}
	if len(s.c.webdav.Users) == 0 {
		return 0
	}
	role, exists := s.c.webdav.Users[username]
	if !exists || role != "read-write" {
		return http.StatusForbidden
	}
	return 0
}

func (s *Server) spoolWebDAVPut(request *http.Request) (*http.Request, error) {
	tempDir := strings.TrimSpace(s.c.webdav.UploadTempDir)
	if tempDir == "" {
		tempDir = os.TempDir()
	}
	if err := os.MkdirAll(tempDir, 0o700); err != nil {
		return nil, fmt.Errorf("create WebDAV upload temp directory: %w", err)
	}
	file, err := os.CreateTemp(tempDir, "tgfile-webdav-*")
	if err != nil {
		return nil, fmt.Errorf("create WebDAV upload spool: %w", err)
	}
	tempPath := file.Name()
	cleanup := func() {
		_ = file.Close()
		_ = os.Remove(tempPath)
	}
	limit := s.c.webdav.MaxUploadSize
	if limit <= 0 {
		limit = 5 * 1024 * 1024 * 1024
	}
	written, err := io.Copy(file, io.LimitReader(request.Body, limit+1))
	closeErr := request.Body.Close()
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("spool WebDAV upload: %w", err)
	}
	if closeErr != nil {
		cleanup()
		return nil, fmt.Errorf("close WebDAV upload request: %w", closeErr)
	}
	if written > limit {
		cleanup()
		return nil, errWebDAVUploadTooLarge
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return nil, fmt.Errorf("rewind WebDAV upload spool: %w", err)
	}
	request.Body = &removeOnCloseFile{
		File: file,
		path: tempPath,
	}
	request.ContentLength = written
	request.TransferEncoding = nil
	return request, nil
}

type removeOnCloseFile struct {
	*os.File
	path string
	once sync.Once
	err  error
}

func (f *removeOnCloseFile) Close() error {
	f.once.Do(func() {
		f.err = errors.Join(f.File.Close(), os.Remove(f.path))
	})
	return f.err
}

func writeWebDAVUploadError(writer http.ResponseWriter, status int) {
	writer.Header().Set("Cache-Control", "private, no-cache")
	writer.Header().Add("Vary", "Authorization")
	writer.WriteHeader(status)
}

func cleanupStaleWebDAVUploads(directory string, olderThan time.Duration) error {
	if strings.TrimSpace(directory) == "" {
		return nil
	}
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read WebDAV upload temp directory: %w", err)
	}
	cutoff := time.Now().Add(-olderThan)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "tgfile-webdav-") {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(directory, entry.Name())); err != nil &&
			!errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove stale WebDAV upload spool: %w", err)
		}
	}
	return nil
}

func (s *Server) newHTTPServer() *http.Server {
	return &http.Server{
		Addr:              s.bind,
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		ReadTimeout:       0,
		WriteTimeout:      0,
		MaxHeaderBytes:    1 << 20,
	}
}

func (s *Server) Run(ctx context.Context) error {
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(ctx, "tcp", s.bind)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.bind, err)
	}
	return s.serve(ctx, listener)
}

func (s *Server) serve(ctx context.Context, listener net.Listener) error {
	httpServer := s.newHTTPServer()
	serveFinished := make(chan struct{})
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		select {
		case <-ctx.Done():
			shutdownContext, cancel := context.WithTimeout(
				context.WithoutCancel(ctx),
				30*time.Second,
			)
			defer cancel()
			_ = httpServer.Shutdown(shutdownContext)
		case <-serveFinished:
		}
	}()

	err := httpServer.Serve(listener)
	close(serveFinished)
	<-shutdownDone
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return fmt.Errorf("serve HTTP: %w", err)
}
