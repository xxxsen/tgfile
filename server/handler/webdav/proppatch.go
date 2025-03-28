package webdav

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

func handlePropPatch(c *gin.Context) {
	c.AbortWithStatus(http.StatusForbidden)
}
