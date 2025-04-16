package file

import (
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/xxxsen/common/webapi/proxyutil"

	"github.com/xxxsen/tgfile/server/model"

	"github.com/gin-gonic/gin"
)

func (h *FileHandler) GetMetaInfo(c *gin.Context) {
	ctx := c.Request.Context()
	key := c.Param("key")
	link, err := h.extractLinkFromFileKey(key)
	if err != nil {
		proxyutil.FailJson(c, http.StatusBadRequest, fmt.Errorf("invalid fkey, err:%w", err))
		return
	}
	info, err := h.m.StatFileLink(ctx, link)
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
	fidinfo, err := h.m.StatFile(ctx, info.FileId)
	if err != nil {
		proxyutil.FailJson(c, http.StatusInternalServerError, fmt.Errorf("read file info fail, err:%w", err))
		return
	}
	proxyutil.SuccessJson(c, &model.GetFileInfoResponse{
		Item: &model.FileInfoItem{
			Key:           key,
			Exist:         true,
			FileSize:      info.FileSize,
			Ctime:         info.Ctime,
			Mtime:         info.Mtime,
			Md5:           fidinfo.Md5Sum,
			FilePartCount: int32(fidinfo.FilePartCount),
		},
	})
}
