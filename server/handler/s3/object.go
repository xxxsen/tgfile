package s3

import (
	"fmt"
	"net/http"
	"strings"
	"tgfile/constant"
	"tgfile/entity"
	"tgfile/filemgr"
	"tgfile/server/handler/s3/s3base"
	"time"

	"github.com/gin-gonic/gin"
)

func DownloadObject(c *gin.Context) {
	ctx := c.Request.Context()
	filename := c.Request.URL.Path
	minfo, err := filemgr.ResolveLink(ctx, filename)
	if err != nil {
		s3base.WriteError(c, http.StatusInternalServerError, fmt.Errorf("get mapping info fail, err:%w", err))
		return
	}
	file, err := filemgr.Open(ctx, minfo.FileId)
	if err != nil {
		s3base.WriteError(c, http.StatusInternalServerError, fmt.Errorf("open file fail, err:%w", err))
		return
	}
	defer file.Close()
	//c.Writer.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s", strconv.Quote()))
	http.ServeContent(c.Writer, c.Request, minfo.Name(), minfo.ModTime(), file)
}

func UploadObject(c *gin.Context) {
	ctx := c.Request.Context()
	filename := c.Request.URL.Path
	fileid, err := filemgr.Create(ctx, c.Request.ContentLength, c.Request.Body)
	if err != nil {
		s3base.WriteError(c, http.StatusInternalServerError, fmt.Errorf("do file upload fail, err:%w", err))
		return
	}
	now := uint64(time.Now().UnixMilli())
	if err := filemgr.CreateLink(ctx, filename, fileid, &entity.CreateLinkOption{
		FileMode: constant.DefaultFileMode,
		IsDir:    strings.HasSuffix(filename, "/"),
		Ctime:    now,
		Mtime:    now,
		FileSize: c.Request.ContentLength,
	}); err != nil {
		s3base.WriteError(c, http.StatusInternalServerError, fmt.Errorf("create mapping fail, err:%w", err))
		return
	}
	//TODO: 确认下, 不写etag是否会有问题
	c.Writer.WriteHeader(http.StatusOK)
}
