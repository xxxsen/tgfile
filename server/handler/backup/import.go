package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"tgfile/filemgr"
	"tgfile/proxyutil"
	"tgfile/server/model"

	"github.com/gin-gonic/gin"
	"github.com/xxxsen/common/logutil"
	"go.uber.org/zap"
)

func Import(c *gin.Context, ctx context.Context, request interface{}) {
	req := request.(*model.ImportRequest)
	header := req.File
	file, err := header.Open()
	if err != nil {
		proxyutil.FailJson(c, http.StatusBadRequest, fmt.Errorf("open file for import fail, err:%w", err))
		return
	}
	gzReader, err := gzip.NewReader(file)
	if err != nil {
		proxyutil.FailJson(c, http.StatusBadRequest, fmt.Errorf("treat file as gz stream fail, err:%w", err))
		return
	}
	defer gzReader.Close()
	// 创建 TAR Reader 解析 tar 结构
	tarReader := tar.NewReader(gzReader)
	var retErr error
	var containStatisticFile bool
	for {
		// 读取下一个文件头
		h, err := tarReader.Next()
		if err == io.EOF {
			break // 读取完毕
		}
		if err != nil {
			retErr = fmt.Errorf("tar read failed, err:%w", err)
			break
		}
		// 仅处理普通文件
		if h.Typeflag != tar.TypeReg {
			continue
		}
		if h.Name == defaultStatisticFileName {
			containStatisticFile = true
			continue
		}

		if err := importOneFile(ctx, h, tarReader); err != nil {
			retErr = fmt.Errorf("import failed, name:%s, size:%d, err:%w", h.Name, h.Size, err)
			break
		}
		logutil.GetLogger(ctx).Info("import one file succ", zap.String("name", h.Name), zap.Int64("size", h.Size))
	}
	if retErr != nil {
		proxyutil.FailJson(c, http.StatusBadRequest, fmt.Errorf("import file failed, err:%w", err))
		return
	}
	if !containStatisticFile {
		proxyutil.FailJson(c, http.StatusBadRequest, fmt.Errorf("no found %s in import file, may be export function not finish", defaultStatisticFileName))
		return
	}
	proxyutil.SuccessJson(c, map[string]interface{}{})
}

func importOneFile(ctx context.Context, h *tar.Header, r *tar.Reader) error {
	limitR := io.LimitReader(r, h.Size)
	fileid, err := filemgr.Create(ctx, h.Size, limitR)
	if err != nil {
		return fmt.Errorf("create file failed, err:%w", err)
	}
	if err := filemgr.CreateLink(ctx, h.Name, fileid, h.Size, false); err != nil {
		return fmt.Errorf("create link failed, err:%w", err)
	}
	return nil
}
