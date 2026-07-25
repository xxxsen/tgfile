package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/xxxsen/common/webapi"
	"github.com/xxxsen/common/webapi/auth"
	"github.com/xxxsen/common/webapi/middleware"
	"github.com/xxxsen/common/webapi/proxyutil"

	"github.com/xxxsen/tgfile/filemgr"
	"github.com/xxxsen/tgfile/server/handler/backup"
	"github.com/xxxsen/tgfile/server/handler/file"
	"github.com/xxxsen/tgfile/server/handler/s3"
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
}

func New(bind string, opts ...Option) (*Server, error) {
	c := applyOpts(opts...)
	svr := &Server{c: c, bind: bind}
	var err error
	svr.engine, err = webapi.NewEngine(
		"/",
		bind,
		webapi.WithAuth(auth.MapUserMatch(c.userMap)),
		webapi.WithRegister(svr.initAPI),
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
	if s.c.s3Enable {
		s3Handler := s3.NewS3Handler(s.c.fmgr)
		for _, bk := range s.c.s3Buckets {
			bucketRouter := router.Group(fmt.Sprintf("/%s", bk))
			bucketRouter.GET("", s3Handler.GetBucket)
			bucketRouter.GET("/*object", s3Handler.DownloadObject)
			bucketRouter.HEAD("/*object", s3Handler.HeadObject)
			bucketRouter.PUT("/*object", mustAuthMiddleware, s3Handler.UploadObject)
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
