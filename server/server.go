package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/xxxsen/common/webapi"
	"github.com/xxxsen/common/webapi/auth"
	"github.com/xxxsen/common/webapi/middleware"
	"github.com/xxxsen/common/webapi/proxyutil"

	"github.com/xxxsen/tgfile/filemgr"
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

type Server struct {
	c      *config
	engine webapi.IWebEngine
	bind   string
	s3     *s3.S3Handler
}

func New(bind string, opts ...Option) (*Server, error) {
	c := applyOpts(opts...)
	svr := &Server{c: c, bind: bind}
	if c.s3.Enabled {
		buckets := make([]s3.Bucket, 0, len(c.s3.Buckets))
		for _, bucket := range c.s3.Buckets {
			buckets = append(buckets, s3.Bucket{
				Name: bucket.Name,
				ACL:  s3.BucketACL(bucket.ACL),
			})
		}
		svr.s3 = s3.NewS3Handler(c.fmgr, s3.Config{
			Buckets:       buckets,
			MaxObjectSize: c.s3.MaxObjectSize,
			Users:         c.userMap,
		})
	}
	var err error
	svr.engine, err = webapi.NewEngine(
		"/",
		bind,
		webapi.WithAuth(auth.MapUserMatch(c.userMap)),
		webapi.WithAuthenticators(auth.NewBasic()),
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

	backupRouter := router.Group("/backup", mustAuthMiddleware)
	{
		backupHandler := backup.NewBackupHandler(s.c.fmgr)
		backupRouter.GET("/export", backupHandler.Export)
		importBackup := proxyutil.WrapBizFunc(
			func(c *gin.Context, ctx context.Context, request any) {
				backupHandler.Import(ctx, c, request)
			},
			&model.ImportRequest{},
		)
		backupRouter.POST("/import", importBackup)
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
			bucketRouter.POST("", s.s3.DeleteObjects)
			bucketRouter.GET("/*object", s.s3.DownloadObject)
			bucketRouter.HEAD("/*object", s.s3.HeadObject)
			bucketRouter.PUT("/*object", s.s3.UploadObject)
			bucketRouter.DELETE("/*object", s.s3.DeleteObject)
		}
	}
	if s.c.webdavEnable {
		webdavRouter := router.Group("/webdav", mustAuthMiddleware)
		{
			webdavHandler := webdav.NewWebdavHandler(s.c.fmgr, s.c.webdavRoot, webdavRouter.BasePath())
			for _, method := range webdav.AllowMethods {
				webdavRouter.Handle(method, "/*all", webdavHandler.Handler)
			}
		}
	}
}

func (s *Server) noRoute(c *gin.Context) {
	if s.s3 == nil {
		c.Status(http.StatusNotFound)
		return
	}
	first, _, _ := strings.Cut(strings.TrimPrefix(c.Request.URL.Path, "/"), "/")
	switch first {
	case "", "file", "static", "backup", "webdav":
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
	s.engine.ServeHTTP(writer, request)
}

func (s *Server) newHTTPServer() *http.Server {
	return &http.Server{
		Addr:              s.bind,
		Handler:           s.engine,
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
