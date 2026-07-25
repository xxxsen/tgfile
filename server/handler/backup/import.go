package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/xxxsen/common/webapi/proxyutil"

	"github.com/xxxsen/tgfile/server/model"

	"github.com/gin-gonic/gin"
	"github.com/xxxsen/common/logutil"
	"go.uber.org/zap"
)

var (
	errInvalidImportRequest   = errors.New("invalid import request")
	errMissingImportStatistic = errors.New("import statistic entry is missing")
)

func (h *BackupHandler) Import(ctx context.Context, c *gin.Context, request any) {
	req, ok := request.(*model.ImportRequest)
	if !ok {
		proxyutil.FailJson(c, http.StatusInternalServerError, errInvalidImportRequest)
		return
	}
	header := req.File
	file, err := header.Open()
	if err != nil {
		proxyutil.FailJson(c, http.StatusBadRequest, fmt.Errorf("open file for import fail, err:%w", err))
		return
	}
	defer logCloseError(ctx, file, "close backup import file")
	gzReader, err := gzip.NewReader(file)
	if err != nil {
		proxyutil.FailJson(c, http.StatusBadRequest, fmt.Errorf("treat file as gz stream fail, err:%w", err))
		return
	}
	defer logCloseError(ctx, gzReader, "close backup gzip reader")
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
		proxyutil.FailJson(
			c,
			http.StatusBadRequest,
			fmt.Errorf("%w: %s", errMissingImportStatistic, defaultStatisticFileName),
		)
		return
	}
	proxyutil.SuccessJson(c, map[string]any{})
}

func logCloseError(ctx context.Context, closer io.Closer, message string) {
	if err := closer.Close(); err != nil {
		logutil.GetLogger(ctx).Error(message, zap.Error(err))
	}
}

func (h *BackupHandler) importOneFile(ctx context.Context, hdr *tar.Header, r *tar.Reader) error {
	limitR := io.LimitReader(r, hdr.Size)
	fileid, err := h.fmgr.CreateFile(ctx, hdr.Size, limitR)
	if err != nil {
		return fmt.Errorf("create file failed, err:%w", err)
	}
	if err := h.fmgr.CreateFileLink(ctx, hdr.Name, fileid, hdr.Size, false); err != nil {
		cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		cleanupErr := h.fmgr.DiscardUnpublishedFile(cleanupContext, fileid)
		cancel()
		return fmt.Errorf("create link failed: %w", errors.Join(err, cleanupErr))
	}
	return nil
}
