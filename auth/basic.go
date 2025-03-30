package auth

import (
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"
)

const (
	BasicAuthName = "basic"
)

func init() {
	register(&basicAuth{})
}

type basicAuth struct {
}

func (b *basicAuth) Name() string {
	return BasicAuthName
}

func (b *basicAuth) IsMatchAuthType(ctx *gin.Context) bool {
	auth := ctx.GetHeader("Authorization")
	return strings.HasPrefix(auth, "Basic")
}

func (b *basicAuth) Auth(ctx *gin.Context, fn UserQueryFunc) (string, error) {
	uak, usk, ok := ctx.Request.BasicAuth()
	if !ok {
		return "", fmt.Errorf("no auth found")
	}

	sk, ok, err := fn(ctx, uak)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("user not found, u:%s", uak)
	}
	if sk != usk {
		return "", fmt.Errorf("sk not match, carry:%s", usk)
	}
	return uak, nil
}
