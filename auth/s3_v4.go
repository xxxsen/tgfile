package auth

import (
	"fmt"

	"github.com/gin-gonic/gin"
	"github.com/xxxsen/s3verify"
)

func init() {
	Regist(S3V4AuthName, func() IAuth {
		return &s3AuthV4{}
	})
}

const (
	S3V4AuthName = "s3_v4"
)

type s3AuthV4 struct {
}

func (c *s3AuthV4) Name() string {
	return S3V4AuthName
}

func (c *s3AuthV4) IsMatchAuthType(ctx *gin.Context) bool {
	return s3verify.IsRequestSignatureV4(ctx.Request)
}

func (c *s3AuthV4) Auth(ctx *gin.Context, users map[string]string) (string, error) {
	ak, ok, err := s3verify.Verify(ctx.Request, users)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("signature not match")
	}
	return ak, nil
}
