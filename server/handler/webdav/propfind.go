package webdav

import (
	"context"
	"mime"
	"net/http"
	"path"
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
	location := c.Request.URL.Path
	var depth int = 0
	if c.GetHeader("Depth") == "1" || c.GetHeader("Depth") == "infinity" { //非0的场景下， 均只获取直接子级
		depth = 1
	}
	base, entries, err := propFindEntries(ctx, location, depth)
	if err != nil {
		logutil.GetLogger(ctx).Error("propfind link failed", zap.String("location", location), zap.Error(err))
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	res := generatePropfindResponse(location, base, entries)
	if err := writeDavResponse(c, res); err != nil {
		logutil.GetLogger(ctx).Error("write as xml failed", zap.Error(err))
		return
	}
}

func propFindEntries(ctx context.Context, location string, depth int) (*entity.FileMappingItem, []*entity.FileMappingItem, error) {
	base, err := filemgr.ResolveLink(ctx, location)
	if err != nil {
		return nil, nil, err
	}

	if !base.IsDir || depth == 0 {
		return base, []*entity.FileMappingItem{base}, nil
	}
	rs := make([]*entity.FileMappingItem, 0, 32)
	if err := filemgr.IterLink(ctx, location, func(ctx context.Context, link string, item *entity.FileMappingItem) (bool, error) {
		rs = append(rs, item)
		return true, nil
	}); err != nil {
		return nil, nil, err
	}
	sort.Slice(rs, func(i, j int) bool {
		left := 0
		right := 0
		if rs[i].IsDir {
			left = 1
		}
		if rs[j].IsDir {
			right = 1
		}
		return left > right
	})
	return base, rs, nil
}

func generatePropfindResponse(location string, base *entity.FileMappingItem, ents []*entity.FileMappingItem) *model.Multistatus {
	ms := &model.Multistatus{
		XMLNS: "DAV:",
	}
	if !base.IsDir { //文件的场景下
		generatePropfindFileResponse(ms, location, base)
		return ms
	}
	//构建目录枚举列表
	generatePropfindDirResponse(ms, location, base, ents)
	return ms
}

func generatePropfindFileResponse(ms *model.Multistatus, location string, base *entity.FileMappingItem) {
	ms.Responses = append(ms.Responses, convertFileMappingItemToResponse(path.Dir(location), base))
}

func generatePropfindDirResponse(ms *model.Multistatus, location string, base *entity.FileMappingItem, ents []*entity.FileMappingItem) {
	{ //处理父目录
		root := path.Dir(strings.TrimSuffix(location, "/"))
		ms.Responses = append(ms.Responses, convertFileMappingItemToResponse(root, base))
	}
	//处理子节点
	root := location
	for _, item := range ents {
		convertFileMappingItemToResponse(root, item)
	}
}

func convertFileMappingItemToResponse(root string, file *entity.FileMappingItem) *model.Response {
	filename := path.Join(root, file.FileName)
	if file.IsDir && !strings.HasSuffix(filename, "/") {
		filename += "/"
	}
	if !file.IsDir {
		filename = strings.TrimSuffix(filename, "/")
	}
	resp := &model.Response{
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
		resp.Propstat.Prop.ResourceType.Collection = " " //不能空
	} else {
		resp.Propstat.Prop.ContentLength = file.FileSize
		contentType := determineMimeType(filename) // 基于扩展名提取文件类型
		resp.Propstat.Prop.ContentType = contentType
	}
	return resp
}

func determineMimeType(name string) string {
	ext := path.Ext(name)
	mimeType := mime.TypeByExtension(ext)
	if mimeType == "" {
		return "application/octet-stream"
	}
	return mimeType
}
