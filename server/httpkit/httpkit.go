package httpkit

import (
	"fmt"
	"path"

	"github.com/gin-gonic/gin"
	"github.com/xxxsen/mimetype"
	"github.com/xxxsen/tgfile/entity"
)

func DetermineMimeType(filename string) string {
	ext := path.Ext(filename)
	mimeType := mimetype.LookupWithDefault(ext, "application/octet-stream")
	return mimeType
}

func SetDefaultDownloadHeader(c *gin.Context, finfo *entity.FileLinkMeta) {
	c.Writer.Header().Set("Content-Type", DetermineMimeType(finfo.FileName))
	c.Writer.Header().Set("Cache-Control", "public, max-age=604800") //默认可以缓存7d
	if finfo.FileId != 0 {
		c.Writer.Header().Set("ETag", fmt.Sprintf("W/\"%d\"", finfo.FileId))
	}
}
