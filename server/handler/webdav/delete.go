package webdav

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

func handleDelete(c *gin.Context) {
	c.AbortWithStatus(http.StatusForbidden)
}
