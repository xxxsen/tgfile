package webdav

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

func Copy(c *gin.Context) {
	c.AbortWithStatus(http.StatusForbidden)
}
