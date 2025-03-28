package webdav

import (
	"fmt"
	"net/http"
	"tgfile/proxyutil"

	"github.com/gin-gonic/gin"
)

func handlePropPatch(c *gin.Context) {
	proxyutil.FailStatus(c, http.StatusForbidden, fmt.Errorf("no impl"))
}
