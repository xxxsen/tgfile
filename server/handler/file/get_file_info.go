package file

import (
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/xxxsen/common/webapi/proxyutil"
	"github.com/xxxsen/tgfile/filemgr"

	"github.com/xxxsen/tgfile/server/model"

	"github.com/gin-gonic/gin"
)

func GetMetaInfo(c *gin.Context) {
	ctx := c.Request.Context()
	key := c.Param("key")
	link, err := extractLinkFromFileKey(key)
	if err != nil {
		proxyutil.FailJson(c, http.StatusBadRequest, fmt.Errorf("invalid fkey, err:%w", err))
		return
	}
	info, err := filemgr.ResolveFileLink(ctx, link)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			proxyutil.SuccessJson(c, model.GetFileInfoResponse{
				Item: &model.FileInfoItem{
					Key:   key,
					Exist: false,
				},
			})
			return
		}
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
