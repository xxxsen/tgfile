package webdav

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

func Propatch(c *gin.Context) {
	c.AbortWithStatus(http.StatusForbidden)
}
