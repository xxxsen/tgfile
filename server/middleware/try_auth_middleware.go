package middleware

import (
	"github.com/xxxsen/tgfile/auth"
	"github.com/xxxsen/tgfile/proxyutil"

	"github.com/gin-gonic/gin"
	"github.com/xxxsen/common/logutil"
	"go.uber.org/zap"
)

func TryAuthMiddleware(users map[string]string) gin.HandlerFunc {
	matchfn := auth.MapUserMatch(users)
	return tryAuthMiddleware(matchfn, auth.AuthList()...)
}

func tryAuthMiddleware(matchfn auth.UserQueryFunc, ats ...auth.IAuth) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		logger := logutil.GetLogger(ctx).With(zap.String("method", c.Request.Method),
			zap.String("path", c.Request.URL.Path), zap.String("ip", c.ClientIP()))

		for _, fn := range ats {
			ak, err := fn.Auth(c, matchfn)
			if err != nil {
				continue
			}
			logger.Debug("user auth succ", zap.String("auth", fn.Name()), zap.String("ak", ak))
			ctx := c.Request.Context()
			ctx = proxyutil.SetUserInfo(ctx, &proxyutil.UserInfo{
				AuthType: fn.Name(),
				Username: ak,
			})
			c.Request = c.Request.WithContext(ctx)
			return
		}
	}
}
