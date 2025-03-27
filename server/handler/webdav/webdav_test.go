package webdav

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"golang.org/x/net/webdav"
)

func TestWebdav(t *testing.T) {
	h := &webdav.Handler{
		FileSystem: webdav.Dir("/home/sen"),
		LockSystem: webdav.NewMemLS(),
	}
	err := http.ListenAndServe(":9991", h)
	assert.NoError(t, err)
}
