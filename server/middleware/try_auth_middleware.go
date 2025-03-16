package middleware

import (
	"fmt"
	"net/http"
	"tgfile/auth"
	"tgfile/proxyutil"

	"github.com/gin-gonic/gin"
	"github.com/xxxsen/common/logutil"
	"go.uber.org/zap"
)

func CreateFullAuthMethods() []auth.IAuth {
	authList := []auth.IAuth{}
	lst := auth.AuthList()
	for _, name := range lst {
		ath := auth.MustCreateByName(name)
		authList = append(authList, ath)
	}
	return authList
}

func TryAuthMiddleware(users map[string]string) gin.HandlerFunc {
	return tryAuthMiddleware(users, CreateFullAuthMethods()...)
}

func tryAuthMiddleware(users map[string]string, ats ...auth.IAuth) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		logger := logutil.GetLogger(ctx).With(zap.String("method", c.Request.Method),
			zap.String("path", c.Request.URL.Path), zap.String("ip", c.ClientIP()))

		for _, fn := range ats {
			if !fn.IsMatchAuthType(c) {
				continue
			}
			ak, err := fn.Auth(c, users)
			if err != nil {
				logger.Error("auth error", zap.String("auth", fn.Name()), zap.Error(err))
				c.AbortWithError(http.StatusUnauthorized, fmt.Errorf("internal services error, err:%w", err))
				return
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
