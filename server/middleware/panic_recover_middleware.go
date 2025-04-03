package middleware

import (
	"fmt"
	"net/http"
	"runtime/debug"

	"github.com/xxxsen/tgfile/proxyutil"

	"github.com/gin-gonic/gin"
	"github.com/xxxsen/common/logutil"
	"go.uber.org/zap"
)

func PanicRecoverMiddleware() gin.HandlerFunc {
	return func(ctx *gin.Context) {
		defer func() {
			if err := recover(); err != nil {
				logutil.GetLogger(ctx).Error("service panic", zap.Any("panic", err), zap.String("path", ctx.Request.URL.Path), zap.String("stack", string(debug.Stack())))
				proxyutil.FailJson(ctx, http.StatusInternalServerError, fmt.Errorf("service panic"))
			}
		}()
		ctx.Next()
	}
}
