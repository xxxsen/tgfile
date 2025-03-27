package webdav

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

func Delete(c *gin.Context) {
	c.AbortWithStatus(http.StatusForbidden)
}
