package webdav

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

func Move(c *gin.Context) {
	c.AbortWithStatus(http.StatusForbidden)
}
