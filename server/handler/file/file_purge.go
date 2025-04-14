package file

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/xxxsen/common/logutil"
	"github.com/xxxsen/common/webapi/proxyutil"
	"go.uber.org/zap"
)

func (h *FileHandler) FilePurge(c *gin.Context) {
	ctx := c.Request.Context()
	cnt, err := h.m.PurgeFile(ctx, nil)
	if err != nil {
		proxyutil.FailJson(c, http.StatusInternalServerError, fmt.Errorf("purge file failed, err:%w", err))
		return
	}
	logutil.GetLogger(ctx).Info("purge file succ", zap.Int64("remove_file_count", cnt))
}
