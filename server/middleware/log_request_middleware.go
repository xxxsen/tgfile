package middleware

import (
	"time"

	"github.com/xxxsen/tgfile/proxyutil"

	"github.com/gin-gonic/gin"
	"github.com/xxxsen/common/logutil"
	"go.uber.org/zap"
)

func LogRequestMiddleware() gin.HandlerFunc {
	return func(ctx *gin.Context) {
		start := time.Now()

		logutil.GetLogger(ctx.Request.Context()).
			With(zap.String("method", ctx.Request.Method),
				zap.String("path", ctx.Request.URL.Path),
				zap.String("ip", ctx.ClientIP())).Info("request start")
		ctx.Next()
		cost := time.Since(start)
		logutil.GetLogger(ctx.Request.Context()).Info("request finish", zap.Error(proxyutil.GetReplyErrInfo(ctx)), zap.Int("status_code", ctx.Writer.Status()), zap.Duration("cost", cost))
	}
}
