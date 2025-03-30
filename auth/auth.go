package auth

import (
	"context"

	"github.com/gin-gonic/gin"
)

type UserQueryFunc func(ctx context.Context, ak string) (string, bool, error)

func MapUserMatch(ud map[string]string) UserQueryFunc {
	return func(ctx context.Context, ak string) (string, bool, error) {
		usk, ok := ud[ak]
		if !ok {
			return "", false, nil
		}
		return usk, true, nil
	}
}

type IAuth interface {
	Name() string
	Auth(ctx *gin.Context, userdata UserQueryFunc) (string, error)
}

var mp = make(map[string]IAuth)

func register(fn IAuth) {
	mp[fn.Name()] = fn
}

func AuthList() []IAuth {
	rs := make([]IAuth, 0, len(mp))
	for _, v := range mp {
		rs = append(rs, v)
	}
	return rs
}
