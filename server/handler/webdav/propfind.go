package webdav

import (
	"context"
	"net/http"
	"path/filepath"
	"strconv"
	"tgfile/entity"
	"tgfile/filemgr"
	"tgfile/server/model"

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
	c.XML(http.StatusMultiStatus, res)
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
	rs := &model.Multistatus{}
	for _, item := range ents {
		rs.Responses = append(rs.Responses, convertFileMappingItemToXmlResponse(prefix, item))
	}
	return rs
}

func convertFileMappingItemToXmlResponse(prefix string, f *entity.FileMappingItem) model.Response {
	prop := model.Prop{
		DisplayName: filepath.Join(prefix, f.FileName),
	}

	if f.IsDir {
		prop.ResourceType = model.ResourceType{Collection: ""}
	} else {
		prop.ContentLength = strconv.FormatInt(f.FileSize, 10)
	}

	return model.Response{
		Href: f.FileName,
		Propstats: []model.Propstat{
			{
				Prop:   prop,
				Status: "HTTP/1.1 200 OK",
			},
		},
	}
}
