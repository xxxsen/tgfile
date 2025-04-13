package httpkit

import (
	"fmt"
	"mime"
	"path"

	"github.com/gin-gonic/gin"
	"github.com/xxxsen/tgfile/entity"
)

func DetermineMimeType(filename string) string {
	ext := path.Ext(filename)
	mimeType := mime.TypeByExtension(ext)
	if mimeType == "" {
		return "application/octet-stream"
	}
	return mimeType
}

func SetDefaultDownloadHeader(c *gin.Context, finfo *entity.FileMappingItem) {
	c.Writer.Header().Set("Content-Type", DetermineMimeType(finfo.FileName))
	c.Writer.Header().Set("Cache-Control", "public, max-age=604800") //默认可以缓存7d
	if finfo.FileId != 0 {
		c.Writer.Header().Set("ETag", fmt.Sprintf("W/\"%d\"", finfo.FileId))
	}
}
