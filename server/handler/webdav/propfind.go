package webdav

import (
	"context"
	"encoding/xml"
	"net/http"
	"path/filepath"
	"tgfile/entity"
	"tgfile/filemgr"
	"tgfile/server/model"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/xxxsen/common/logutil"
	"go.uber.org/zap"
)

// 部分代码参考: https://github.com/emersion/go-webdav

func handlePropfind(c *gin.Context) {
	ctx := c.Request.Context()
	file := c.Request.URL.Path
	entries, prefix, err := propFindEntries(ctx, file)
	if err != nil {
		logutil.GetLogger(ctx).Error("propfind link failed", zap.String("link", file), zap.Error(err))
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	res := generatePropfindResponse(prefix, entries)
	if err := writeAsXml(c, res); err != nil {
		logutil.GetLogger(ctx).Error("write as xml failed", zap.Error(err))
	}
}

func writeAsXml(c *gin.Context, res *model.Multistatus) error {
	res.XMLNS = "DAV:"
	c.Status(http.StatusMultiStatus)
	if _, err := c.Writer.Write([]byte(xml.Header)); err != nil {
		return err
	}
	raw, err := xml.Marshal(res)
	if err != nil {
		return err
	}
	if _, err := c.Writer.Write(raw); err != nil {
		return err
	}
	return nil
}

func propFindEntries(ctx context.Context, filename string) ([]*entity.FileMappingItem, string, error) {
	info, err := filemgr.ResolveLink(ctx, filename)
	if err != nil {
		return nil, "", err
	}

	if !info.IsDir {
		return []*entity.FileMappingItem{info}, filepath.Dir(filename), nil
	}
	rs := make([]*entity.FileMappingItem, 0, 32)
	if err := filemgr.IterLink(ctx, filename, func(ctx context.Context, link string, item *entity.FileMappingItem) (bool, error) {
		rs = append(rs, item)
		return true, nil
	}); err != nil {
		return nil, "", err
	}
	return rs, filename, nil
}

func generatePropfindResponse(prefix string, ents []*entity.FileMappingItem) *model.Multistatus {
	return convertToMultistatus(ents, prefix)
}

func convertToMultistatus(files []*entity.FileMappingItem, basePath string) *model.Multistatus {
	responses := []model.Response{}

	for _, file := range files {
		resp := model.Response{
			Href: filepath.Join(basePath, file.FileName),
			Propstat: model.Propstat{
				Prop: model.Prop{
					DisplayName:  file.FileName,
					LastModified: time.UnixMilli(file.Mtime).Format(http.TimeFormat),
					ResourceType: model.ResourceType{},
				},
				Status: "HTTP/1.1 200 OK",
			},
		}

		if file.IsDir {
			resp.Propstat.Prop.ResourceType.Collection = " "
		} else {
			resp.Propstat.Prop.ContentLength = file.FileSize
			contentType := "application/octet-stream" // 默认文件类型
			resp.Propstat.Prop.ContentType = contentType
		}

		responses = append(responses, resp)
	}

	return &model.Multistatus{Responses: responses}
}
