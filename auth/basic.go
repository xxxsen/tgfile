package auth

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"
)

const (
	BasicAuthName = "basic"
)

func init() {
	Regist(BasicAuthName, func() IAuth {
		return &basicAuth{}
	})
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

func (b *basicAuth) Auth(ctx *gin.Context, users map[string]string) (string, error) {
	auth := ctx.GetHeader("Authorization")
	if len(auth) == 0 {
		return "", fmt.Errorf("authorization key not found")
	}
	authData := strings.SplitN(auth, " ", 2)
	if len(authData) != 2 {
		return "", fmt.Errorf("invalid auth data:%s", auth)
	}
	if authData[0] != "Basic" {
		return "", fmt.Errorf("authorization value should startswith Basic, v:%s", authData[0])
	}
	bdata, err := base64.StdEncoding.DecodeString(authData[1])
	if err != nil {
		return "", fmt.Errorf("decode auth data fail, data:%s, err:%w", authData[1], err)
	}
	data := string(bdata)
	userdata := strings.SplitN(data, ":", 2)
	if len(userdata) != 2 {
		return "", fmt.Errorf("invalid user pwd data:%s", data)
	}
	sk, ok := users[userdata[0]]
	if !ok {
		return "", fmt.Errorf("user not found, u:%s", userdata[0])
	}
	if sk != userdata[1] {
		return "", fmt.Errorf("sk not match, carry:%s", userdata[1])
	}
	return userdata[0], nil
}
