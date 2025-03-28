package webdav

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCheckSameRoot(t *testing.T) {
	assert.True(t, checkSameWebdavRoot("/webdav/hello/world", "/webdav/aaaa"))
	assert.True(t, checkSameWebdavRoot("/webdav/1232/world", "/webdav/32424"))
	assert.False(t, checkSameWebdavRoot("/webdav/1232/world", "/11/32424"))
	assert.False(t, checkSameWebdavRoot("/webdav/1232/world", "/32424"))
}
