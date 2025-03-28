package webdav

import (
	"context"
	"fmt"
	"net/http"
	"path"
	"tgfile/entity"
	"tgfile/filemgr"

	"github.com/gin-gonic/gin"
	"github.com/xxxsen/common/logutil"
	"go.uber.org/zap"
)

func handleDelete(c *gin.Context) {
	ctx := c.Request.Context()
	root := c.Request.URL.Path
	info, err := filemgr.ResolveLink(ctx, root)
	if err != nil {
		logutil.GetLogger(ctx).Error("read link info failed", zap.String("link", root), zap.Error(err))
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	handler := handleDeleteFile
	if info.IsDir {
		handler = handleDeleteDir
	}
	if err := handler(ctx, root, info); err != nil {
		logutil.GetLogger(ctx).Error("handle delete link failed", zap.String("link", root),
			zap.Bool("is_dir", info.IsDir), zap.Error(err))
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	c.AbortWithStatus(http.StatusForbidden)
}

func handleDeleteFile(ctx context.Context, root string, ent *entity.FileMappingItem) error {
	return filemgr.RemoveLink(ctx, root)
}

func handleDeleteDir(ctx context.Context, root string, ent *entity.FileMappingItem) error {
	items := make([]*entity.FileMappingItem, 0, 32)
	if err := filemgr.IterLink(ctx, root, func(ctx context.Context, link string, item *entity.FileMappingItem) (bool, error) {
		items = append(items, item)
		return true, nil
	}); err != nil {
		return fmt.Errorf("iter link:%s and delete failed, err:%w", root, err)
	}
	//递归删除子文件/目录
	for _, item := range items {
		location := path.Join(root, item.FileName)
		if !item.IsDir {
			if err := handleDeleteFile(ctx, location, item); err != nil {
				return fmt.Errorf("delete file link failed, link:%s, err:%w", location, err)
			}
			continue
		}
		if err := handleDeleteDir(ctx, location, item); err != nil {
			return fmt.Errorf("delete dir link failed, link:%s, err:%w", location, err)
		}
	}
	//删除自身
	if err := filemgr.RemoveLink(ctx, root); err != nil {
		return fmt.Errorf("delete self dir link failed, link:%s, err:%w", root, err)
	}
	return nil
}
