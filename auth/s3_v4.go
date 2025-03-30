package auth

import (
	"fmt"

	"github.com/gin-gonic/gin"
	"github.com/xxxsen/s3verify"
)

func init() {
	register(&s3AuthV4{})
}

const (
	S3V4AuthName = "s3_v4"
)

type s3AuthV4 struct {
}

func (c *s3AuthV4) Name() string {
	return S3V4AuthName
}

func (c *s3AuthV4) Auth(ctx *gin.Context, fn UserQueryFunc) (string, error) {
	ak, ok, err := s3verify.Verify(ctx.Request.Context(), ctx.Request, s3verify.UserQueryFunc(fn))
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("signature not match")
	}
	return ak, nil
}
