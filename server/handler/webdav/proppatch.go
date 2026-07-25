package webdav

import (
	"net/http"

	"github.com/xxxsen/common/webapi/proxyutil"

	"github.com/gin-gonic/gin"
)

func (h *WebdavHandler) handlePropPatch(c *gin.Context) {
	proxyutil.FailStatus(c, http.StatusForbidden, errPropertyPatchMissing)
}
