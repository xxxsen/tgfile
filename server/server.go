package server

import (
	"context"
	"fmt"
	"net/http"

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
}

func New(bind string, opts ...Option) (*Server, error) {
	c := applyOpts(opts...)
	svr := &Server{c: c}
	var err error
	svr.engine, err = webapi.NewEngine("/", bind, webapi.WithAuth(auth.MapUserMatch(c.userMap)), webapi.WithRegister(svr.initAPI))
	if err != nil {
		return nil, err
	}
	return svr, nil
}

func (s *Server) initAPI(router *gin.RouterGroup) {
	mustAuthMiddleware := middleware.MustAuthMiddleware()
	fileRouter := router.Group("/file")
	{
		fileRouter.POST("/upload", mustAuthMiddleware, proxyutil.WrapBizFunc(file.FileUpload, &model.UploadFileRequest{}))
		fileRouter.GET("/download/:key", file.FileDownload)
		fileRouter.GET("/meta/:key", file.GetMetaInfo)
		fileRouter.POST("/purge", mustAuthMiddleware, file.FilePurge)
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
	if s.c.s3Enable {
		for _, bk := range s.c.s3Buckets {
			bucketRouter := router.Group(fmt.Sprintf("/%s", bk))
			bucketRouter.GET("", s3.GetBucket)
			bucketRouter.GET("/*object", s3.DownloadObject)
			bucketRouter.PUT("/*object", mustAuthMiddleware, s3.UploadObject)
		}

	}
	if s.c.webdavEnable {
		webdavRouter := router.Group("/webdav", mustAuthMiddleware)
		{
			for _, method := range webdav.AllowMethods {
				webdavRouter.Handle(method, "/*all", webdav.Handler(s.c.webdavRoot, webdavRouter.BasePath()))
			}
		}
	}
}
func (s *Server) Run() error {
	return s.engine.Run()
}
