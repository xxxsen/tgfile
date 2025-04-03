package middleware

import (
	"net/http"

	"github.com/xxxsen/tgfile/proxyutil"

	"github.com/gin-gonic/gin"
)

func MustAuthMiddleware() gin.HandlerFunc {
	return func(ctx *gin.Context) {
		_, ok := proxyutil.GetUserInfo(ctx.Request.Context())
		if !ok {
			ctx.Header("WWW-Authenticate", `Basic realm="Restricted Area"`)
			ctx.AbortWithStatus(http.StatusUnauthorized)
			return
		}

	}
}
