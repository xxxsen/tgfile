package file

import (
	"fmt"
	"net/http"

	"github.com/xxxsen/tgfile/filemgr"
	"github.com/xxxsen/tgfile/proxyutil"

	"github.com/xxxsen/tgfile/server/model"

	"github.com/gin-gonic/gin"
)

func GetMetaInfo(c *gin.Context) {
	ctx := c.Request.Context()
	key := c.Param("key")
	link := defaultUploadPrefix + key

	info, err := filemgr.ResolveLink(ctx, link)
	if err != nil {
		proxyutil.FailJson(c, http.StatusInternalServerError, fmt.Errorf("read file info fail, err:%w", err))
		return
	}
	proxyutil.SuccessJson(c, &model.GetFileInfoResponse{
		Item: &model.FileInfoItem{
			Key:      key,
			Exist:    true,
			FileSize: info.FileSize,
			Ctime:    info.Ctime,
			Mtime:    info.Mtime,
		},
	})
}
