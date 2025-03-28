package webdav

import (
	"fmt"
	"net/http"
	"tgfile/proxyutil"

	"github.com/gin-gonic/gin"
)

func handleCopy(c *gin.Context) {
	proxyutil.FailStatus(c, http.StatusForbidden, fmt.Errorf("no impl"))
}
