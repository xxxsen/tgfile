package file

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/xxxsen/common/logutil"
	"github.com/xxxsen/common/webapi/proxyutil"
	"github.com/xxxsen/tgfile/filemgr"
	"go.uber.org/zap"
)

func FilePurge(c *gin.Context) {
	ctx := c.Request.Context()
	cnt, err := filemgr.PurgeFile(ctx, nil)
	if err != nil {
		proxyutil.FailJson(c, http.StatusInternalServerError, fmt.Errorf("purge file failed, err:%w", err))
		return
	}
	logutil.GetLogger(ctx).Info("purge file succ", zap.Int64("remove_file_count", cnt))
}
