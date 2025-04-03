package webdav

import (
	"fmt"
	"net/http"

	"github.com/xxxsen/tgfile/proxyutil"

	"github.com/gin-gonic/gin"
)

func (h *webdavHandler) handlePropPatch(c *gin.Context) {
	proxyutil.FailStatus(c, http.StatusForbidden, fmt.Errorf("no impl"))
}
