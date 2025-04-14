package webdav

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/xxxsen/common/webapi/proxyutil"
	"github.com/xxxsen/tgfile/entity"
	"github.com/xxxsen/tgfile/filemgr"
	"github.com/xxxsen/tgfile/server/httpkit"
	"github.com/xxxsen/tgfile/server/model"

	"github.com/gin-gonic/gin"
	"github.com/xxxsen/common/logutil"
	"go.uber.org/zap"
)

// 部分代码参考: https://github.com/emersion/go-webdav

func (h *webdavHandler) handlePropfind(c *gin.Context) {
	ctx := c.Request.Context()
	location := h.buildSrcPath(c)
	var depth int = 0
	if c.GetHeader("Depth") == "1" || c.GetHeader("Depth") == "infinity" { //非0的场景下， 均只获取直接子级
		depth = 1
	}
	base, entries, err := h.propFindEntries(ctx, location, depth)
	if errors.Is(err, os.ErrNotExist) {
		proxyutil.FailStatus(c, http.StatusNotFound, err)
		return
	}

	if err != nil {
		proxyutil.FailStatus(c, http.StatusInternalServerError, fmt.Errorf("find entries failed, location:%s, depth:%d, err:%w", location, depth, err))
		return
	}
	//构建最终返回的时候, 需要将地址转换为用户可见的路径
	// 所以这里实际上应该返回的url的Path
	userLocation := c.Request.URL.Path
	res := h.generatePropfindResponse(userLocation, base, entries)
	if err := h.writeDavResponse(c, res); err != nil {
		logutil.GetLogger(ctx).Error("write as xml failed", zap.Error(err))
		return
	}
}

func (h *webdavHandler) propFindEntries(ctx context.Context, location string, depth int) (*entity.FileMappingItem, []*entity.FileMappingItem, error) {
	base, err := filemgr.ResolveFileLink(ctx, location)
	if err != nil {
		return nil, nil, err
	}

	if !base.IsDir || depth == 0 {
		return base, []*entity.FileMappingItem{}, nil
	}
	rs := make([]*entity.FileMappingItem, 0, 32)
	if err := filemgr.WalkFileLink(ctx, location, func(ctx context.Context, link string, item *entity.FileMappingItem) (bool, error) {
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

func (h *webdavHandler) generatePropfindResponse(location string, base *entity.FileMappingItem, ents []*entity.FileMappingItem) *model.Multistatus {
	ms := &model.Multistatus{
		XMLNS: "DAV:",
	}
	if !base.IsDir { //文件的场景下
		h.generatePropfindFileResponse(ms, location, base)
		return ms
	}
	//构建目录枚举列表
	h.generatePropfindDirResponse(ms, location, base, ents)
	return ms
}

func (h *webdavHandler) generatePropfindFileResponse(ms *model.Multistatus, location string, base *entity.FileMappingItem) {
	ms.Responses = append(ms.Responses, h.convertFileMappingItemToResponse(path.Dir(location), base))
}

func (h *webdavHandler) generatePropfindDirResponse(ms *model.Multistatus, location string, base *entity.FileMappingItem, ents []*entity.FileMappingItem) {
	{ //处理父目录
		ms.Responses = append(ms.Responses, h.convertLastDirFileMappingItemToResponse(location, base))
	}
	//处理子节点
	for _, item := range ents {
		ms.Responses = append(ms.Responses, h.convertFileMappingItemToResponse(location, item))
	}
}

func (h *webdavHandler) convertLastDirFileMappingItemToResponse(root string, file *entity.FileMappingItem) *model.Response {
	if !strings.HasSuffix(root, "/") {
		root += "/"
	}
	return &model.Response{
		Href: root,
		Propstat: model.Propstat{
			Prop: model.Prop{
				DisplayName:  file.FileName,
				LastModified: time.UnixMilli(file.Mtime).Format(http.TimeFormat),
				ResourceType: model.ResourceType{
					Collection: " ",
				},
			},
			Status: "HTTP/1.1 200 OK",
		},
	}
}

func (h *webdavHandler) convertFileMappingItemToResponse(root string, file *entity.FileMappingItem) *model.Response {
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
		contentType := httpkit.DetermineMimeType(filename) // 基于扩展名提取文件类型
		resp.Propstat.Prop.ContentType = contentType
	}
	return resp
}
