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

	// handler here
	fileHandler := file.NewFileHandler(s.c.fmgr)

	fileRouter := router.Group("/file")
	{

		fileRouter.POST("/upload", mustAuthMiddleware, proxyutil.WrapBizFunc(fileHandler.FileUpload, &model.UploadFileRequest{}))
		fileRouter.GET("/download/:key", fileHandler.FileDownload)
		fileRouter.GET("/meta/:key", fileHandler.GetMetaInfo)
		fileRouter.POST("/purge", mustAuthMiddleware, fileHandler.FilePurge)
	}
	multiPartRouter := fileRouter.Group("/multipart")
	{
		multiPartRouter.POST("/begin", mustAuthMiddleware, proxyutil.WrapBizFunc(fileHandler.BeginUpload, &model.BeginUploadRequest{}))
		multiPartRouter.POST("/part", mustAuthMiddleware, proxyutil.WrapBizFunc(fileHandler.PartUpload, &model.PartUploadRequest{}))
		multiPartRouter.POST("/end", mustAuthMiddleware, proxyutil.WrapBizFunc(fileHandler.FinishUpload, &model.FinishUploadRequest{}))
	}
	staticRouter := router.Group("/static", mustAuthMiddleware)
	{
		staticRouter.StaticFS("", http.FS(filemgr.ToFileSystem(context.Background(), s.c.fmgr)))
	}

	backupRouter := router.Group("/backup", mustAuthMiddleware)
	{
		backupHandler := backup.NewBackupHandler(s.c.fmgr)
		backupRouter.GET("/export", backupHandler.Export)
		backupRouter.POST("/import", proxyutil.WrapBizFunc(backupHandler.Import, &model.ImportRequest{}))
	}
	if s.c.s3Enable {
		s3Handler := s3.NewS3Handler(s.c.fmgr)
		for _, bk := range s.c.s3Buckets {
			bucketRouter := router.Group(fmt.Sprintf("/%s", bk))
			bucketRouter.GET("", s3Handler.GetBucket)
			bucketRouter.GET("/*object", s3Handler.DownloadObject)
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
func (s *Server) Run() error {
	return s.engine.Run()
}
