package file

import (
	"fmt"
	"net/http"
	"tgfile/filemgr"
	"tgfile/proxyutil"
	"tgfile/server/model"

	"github.com/gin-gonic/gin"
)

func GetMetaInfo(c *gin.Context) {
	ctx := c.Request.Context()
	key := c.Param("key")
	link := defaultUploadPrefix + key

	info, err := filemgr.ResolveLink(ctx, link)
	if err != nil {
		proxyutil.Fail(c, http.StatusInternalServerError, fmt.Errorf("read file info fail, err:%w", err))
		return
	}
	proxyutil.Success(c, &model.GetFileInfoResponse{
		Item: &model.FileInfoItem{
			Key:      key,
			Exist:    true,
			FileSize: info.FileSize,
			Ctime:    info.Ctime,
			Mtime:    info.Mtime,
		},
	})
}
