package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/xxxsen/common/webapi/proxyutil"
	"github.com/xxxsen/tgfile/server/model"

	"github.com/gin-gonic/gin"
	"github.com/xxxsen/common/logutil"
	"go.uber.org/zap"
)

func (h *BackupHandler) Import(c *gin.Context, ctx context.Context, request interface{}) {
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
		hdr, err := tarReader.Next()
		if err == io.EOF {
			break // 读取完毕
		}
		if err != nil {
			retErr = fmt.Errorf("tar read failed, err:%w", err)
			break
		}
		// 仅处理普通文件
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if hdr.Name == defaultStatisticFileName {
			containStatisticFile = true
			continue
		}

		if err := h.importOneFile(ctx, hdr, tarReader); err != nil {
			retErr = fmt.Errorf("import failed, name:%s, size:%d, err:%w", hdr.Name, hdr.Size, err)
			break
		}
		logutil.GetLogger(ctx).Info("import one file succ", zap.String("name", hdr.Name), zap.Int64("size", hdr.Size))
	}
	if retErr != nil {
		proxyutil.FailJson(c, http.StatusBadRequest, fmt.Errorf("import file failed, err:%w", retErr))
		return
	}
	if !containStatisticFile {
		proxyutil.FailJson(c, http.StatusBadRequest, fmt.Errorf("no found %s in import file, may be export function not finish", defaultStatisticFileName))
		return
	}
	proxyutil.SuccessJson(c, map[string]interface{}{})
}

func (h *BackupHandler) importOneFile(ctx context.Context, hdr *tar.Header, r *tar.Reader) error {
	limitR := io.LimitReader(r, hdr.Size)
	fileid, err := h.fmgr.CreateFile(ctx, hdr.Size, limitR)
	if err != nil {
		return fmt.Errorf("create file failed, err:%w", err)
	}
	if err := h.fmgr.CreateFileLink(ctx, hdr.Name, fileid, hdr.Size, false); err != nil {
		return fmt.Errorf("create link failed, err:%w", err)
	}
	return nil
}
