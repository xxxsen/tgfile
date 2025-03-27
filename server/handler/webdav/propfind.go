package webdav

import (
	"context"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
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
	var depth int = 0
	if c.GetHeader("Depth") == "1" {
		depth = 1
	}
	logutil.GetLogger(ctx).Debug("get propfind request", zap.String("file", file), zap.Int("depth", depth))
	entries, prefix, err := propFindEntries(ctx, file, depth)
	if err != nil {
		logutil.GetLogger(ctx).Error("propfind link failed", zap.String("link", file), zap.Error(err))
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	res := generatePropfindResponse(prefix, entries)
	if err := writeDavResponse(c, res); err != nil {
		logutil.GetLogger(ctx).Error("write as xml failed", zap.Error(err))
		return
	}
	logutil.GetLogger(ctx).Debug("return resource count", zap.Int("count", len(entries)))

}

func propFindEntries(ctx context.Context, filename string, depth int) ([]*entity.FileMappingItem, string, error) {
	info, err := filemgr.ResolveLink(ctx, filename)
	if err != nil {
		return nil, "", err
	}

	if !info.IsDir || depth == 0 {
		return []*entity.FileMappingItem{info}, filepath.Dir(filename), nil
	}
	rs := make([]*entity.FileMappingItem, 0, 32)
	//TODO: 优化这里
	rs = append(rs, info) //必须包含自身
	if err := filemgr.IterLink(ctx, filename, func(ctx context.Context, link string, item *entity.FileMappingItem) (bool, error) {
		rs = append(rs, item)
		return true, nil
	}); err != nil {
		return nil, "", err
	}
	//TODO: 优化这里
	sort.Slice(rs[1:], func(i, j int) bool {
		left := 0
		right := 0
		if rs[i].IsDir {
			left = 1
		}
		if rs[j].IsDir {
			right = 1
		}
		return left < right
	})
	return rs, filename, nil
}

func generatePropfindResponse(prefix string, ents []*entity.FileMappingItem) *model.Multistatus {
	return convertToMultistatus(ents, prefix)
}

func convertToMultistatus(files []*entity.FileMappingItem, basePath string) *model.Multistatus {
	responses := []model.Response{}
	//TODO: 优化这块代码, webdav如果为目录且depth为1的场景下, 那么需要将目录自身也加到返回的列表中
	for idx, file := range files {
		filename := file.FileName
		if idx > 0 {
			filename = filepath.Join(basePath, file.FileName)
		}
		if file.IsDir && !strings.HasSuffix(filename, "/") {
			filename += "/"
		}
		resp := model.Response{
			Href: filename,
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

	return &model.Multistatus{Responses: responses, XMLNS: "DAV:"}
}
