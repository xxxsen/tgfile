package webdav

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

func handleMove(c *gin.Context) {
	c.AbortWithStatus(http.StatusForbidden)
}
