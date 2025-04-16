package webdav

import (
	"fmt"
	"net/http"

	"github.com/xxxsen/common/webapi/proxyutil"

	"github.com/gin-gonic/gin"
)

func (h *WebdavHandler) handlePropPatch(c *gin.Context) {
	proxyutil.FailStatus(c, http.StatusForbidden, fmt.Errorf("no impl"))
}
