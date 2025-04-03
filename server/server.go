package server

import (
	"context"
	"fmt"
	"net/http"

	"github.com/xxxsen/tgfile/filemgr"
	"github.com/xxxsen/tgfile/proxyutil"
	"github.com/xxxsen/tgfile/server/handler/backup"
	"github.com/xxxsen/tgfile/server/handler/file"
	"github.com/xxxsen/tgfile/server/handler/s3"
	"github.com/xxxsen/tgfile/server/handler/webdav"
	"github.com/xxxsen/tgfile/server/middleware"
	"github.com/xxxsen/tgfile/server/model"

	"github.com/gin-gonic/gin"
)

func init() {
	gin.SetMode(gin.ReleaseMode)
}

type Server struct {
	addr   string
	c      *config
	engine *gin.Engine
}

func New(bind string, opts ...Option) (*Server, error) {
	c := applyOpts(opts...)
	svr := &Server{addr: bind, c: c}
	if err := svr.init(); err != nil {
		return nil, err
	}
	return svr, nil
}

func (s *Server) init() error {
	s.engine = gin.New()
	s.initMiddleware(s.engine)
	s.initAPI(s.engine)
	return nil
}

func (s *Server) initMiddleware(router *gin.Engine) {
	mds := []gin.HandlerFunc{
		middleware.PanicRecoverMiddleware(),
		middleware.TraceMiddleware(),
		middleware.LogRequestMiddleware(),
		middleware.TryAuthMiddleware(s.c.userMap),
		middleware.NonLengthIOLimitMiddleware(),
	}
	router.Use(mds...)
}

func (s *Server) initAPI(router *gin.Engine) {
	mustAuthMiddleware := middleware.MustAuthMiddleware()
	fileRouter := router.Group("/file")
	{
		fileRouter.POST("/upload", mustAuthMiddleware, proxyutil.WrapBizFunc(file.FileUpload, &model.UploadFileRequest{}))
		fileRouter.GET("/download/:key", file.FileDownload)
		fileRouter.GET("/meta/:key", file.GetMetaInfo)
	}
	multiPartRouter := fileRouter.Group("/multipart")
	{
		multiPartRouter.POST("/begin", mustAuthMiddleware, proxyutil.WrapBizFunc(file.BeginUpload, &model.BeginUploadRequest{}))
		multiPartRouter.POST("/part", mustAuthMiddleware, proxyutil.WrapBizFunc(file.PartUpload, &model.PartUploadRequest{}))
		multiPartRouter.POST("/end", mustAuthMiddleware, proxyutil.WrapBizFunc(file.FinishUpload, &model.FinishUploadRequest{}))
	}
	staticRouter := router.Group("/static", mustAuthMiddleware)
	{
		staticRouter.StaticFS("", http.FS(filemgr.AsFileSystem(context.Background())))
	}

	backupRouter := router.Group("/backup", mustAuthMiddleware)
	{
		backupRouter.GET("/export", backup.Export)
		backupRouter.POST("/import", proxyutil.WrapBizFunc(backup.Import, &model.ImportRequest{}))
	}
	{
		for _, bk := range s.c.s3Buckets {
			bucketRouter := router.Group(fmt.Sprintf("/%s", bk))
			bucketRouter.GET("", s3.GetBucket)
			bucketRouter.GET("/*object", s3.DownloadObject)
			bucketRouter.PUT("/*object", mustAuthMiddleware, s3.UploadObject)
		}
	}
	if s.c.webdav {
		webdavRouter := router.Group("/webdav", mustAuthMiddleware)
		{
			for _, method := range webdav.AllowMethods {
				webdavRouter.Handle(method, "/*all", webdav.Handler(s.c.webdavRoot, webdavRouter.BasePath()))
			}
		}
	}
}
func (s *Server) Run() error {
	return s.engine.Run(s.addr)
}
