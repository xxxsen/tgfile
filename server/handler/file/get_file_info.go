package file

import (
	"fmt"
	"net/http"
	"tgfile/filemgr"
	"tgfile/proxyutil"
	"tgfile/server/model"
	"tgfile/utils"

	"github.com/gin-gonic/gin"
)

func GetMetaInfo(c *gin.Context) {
	ctx := c.Request.Context()
	key := c.Param("key")
	fileid, err := utils.DecodeFileId(key)
	if err != nil {
		proxyutil.Fail(c, http.StatusBadRequest, fmt.Errorf("decode down key fail, err:%w", err))
		return
	}
	info, err := filemgr.Stat(ctx, fileid)
	if err != nil {
		proxyutil.Fail(c, http.StatusInternalServerError, fmt.Errorf("read file info fail, err:%w", err))
		return
	}
	proxyutil.Success(c, &model.GetFileInfoResponse{
		Item: &model.FileInfoItem{
			Key:      key,
			Exist:    true,
			FileSize: info.Size(),
			Ctime:    info.ModTime().UnixMilli(),
			Mtime:    info.ModTime().UnixMilli(),
		},
	})
}
