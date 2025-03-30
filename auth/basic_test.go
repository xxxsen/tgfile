package auth

import (
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func TestBasicAuth(t *testing.T) {
	at := &basicAuth{}
	r, err := http.NewRequest(http.MethodGet, "http://127.0.0.1/test", nil)
	assert.NoError(t, err)
	ak := "abc"
	sk := "123456"
	r.SetBasicAuth(ak, sk)
	{
		users := map[string]string{
			"test": "123456",
			"abc":  "123456",
		}
		ckak, err := at.Auth(&gin.Context{
			Request: r,
		}, MapUserMatch(users))
		assert.NoError(t, err)
		assert.Equal(t, ak, ckak)
	}
	{
		users := map[string]string{
			"test": "123456",
			"abc":  "1234567",
		}
		ckak, err := at.Auth(&gin.Context{
			Request: r,
		}, MapUserMatch(users))
		assert.Error(t, err)
		assert.NotEqual(t, ak, ckak)
	}
}
