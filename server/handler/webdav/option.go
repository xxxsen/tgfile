package webdav

import (
	"strings"

	"github.com/gin-gonic/gin"
)

func (h *WebdavHandler) handleOption(c *gin.Context) {
	c.Writer.Header().Set("Allow", strings.Join(AllowMethods, ", "))
	c.Writer.Header().Set("DAV", "1")
}
