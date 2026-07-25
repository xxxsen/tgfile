package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"time"

	"github.com/xxxsen/tgfile/filemgr"
	"github.com/xxxsen/tgfile/server/model"

	"github.com/gin-gonic/gin"
	"github.com/xxxsen/common/logutil"
	"go.uber.org/zap"
)

// Export 将s3数据导出
func (h *BackupHandler) Export(c *gin.Context) {
	ctx := c.Request.Context()
	// mdzz, 加了文件头了, 火狐直接给解压了...
	// c.Writer.Header().Set("Content-Encoding", "gzip")
	// c.Writer.Header().Set("Content-Type", "application/tar+gzip")
	filename := fmt.Sprintf("attachment; filename=export.%d.tar.gz", time.Now().UnixMilli())
	c.Writer.Header().Set("Content-Disposition", filename)
	gz := gzip.NewWriter(c.Writer)
	tw := tar.NewWriter(gz)
	st := &model.StatisticInfo{}
	start := time.Now()
	if err := fs.WalkDir(filemgr.ToFileSystem(ctx, h.fmgr), "/", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walk export path %q: %w", path, err)
		}
		if d.IsDir() {
			return nil
		}
		return h.exportOneFile(ctx, tw, st, path)
	}); err != nil {
		logutil.GetLogger(ctx).Error("iter link failed", zap.Error(err))
		h.closeExportWriters(ctx, tw, gz)
		return
	}
	cost := time.Since(start)
	st.TimeCost = cost.Milliseconds()
	if err := h.writeStatistic(tw, st); err != nil {
		logutil.GetLogger(ctx).Error("write export statistic info failed", zap.Error(err))
		h.closeExportWriters(ctx, tw, gz)
		return
	}
	h.closeExportWriters(ctx, tw, gz)
	logutil.GetLogger(ctx).Info("iter link and export succ")
}

func (h *BackupHandler) exportOneFile(
	ctx context.Context,
	writer *tar.Writer,
	statistic *model.StatisticInfo,
	filePath string,
) error {
	entry, err := h.fmgr.StatFileLink(ctx, filePath)
	if err != nil {
		return fmt.Errorf("stat export path %q: %w", filePath, err)
	}
	stream, err := h.fmgr.OpenFile(ctx, entry.FileId)
	if err != nil {
		return fmt.Errorf("open export file %d: %w", entry.FileId, err)
	}
	header := &tar.Header{
		Name: filePath,
		Mode: int64(entry.Mode),
		Size: entry.FileSize,
	}
	if err := writer.WriteHeader(header); err != nil {
		return errors.Join(
			fmt.Errorf("write export header for file %d: %w", entry.FileId, err),
			stream.Close(),
		)
	}
	_, copyErr := io.Copy(writer, stream)
	closeErr := stream.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return fmt.Errorf("write export body for file %d: %w", entry.FileId, err)
	}
	statistic.FileCount++
	statistic.FileSize += entry.FileSize
	logutil.GetLogger(ctx).Debug(
		"iter one link succ",
		zap.String("link", filePath),
		zap.Uint64("file_id", entry.FileId),
	)
	return nil
}

func (h *BackupHandler) closeExportWriters(ctx context.Context, tarWriter *tar.Writer, gzipWriter *gzip.Writer) {
	if err := errors.Join(tarWriter.Close(), gzipWriter.Close()); err != nil {
		logutil.GetLogger(ctx).Error("close export archive", zap.Error(err))
	}
}

func (h *BackupHandler) writeStatistic(w *tar.Writer, st *model.StatisticInfo) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("encode export statistic: %w", err)
	}
	if err := w.WriteHeader(&tar.Header{
		Name: defaultStatisticFileName,
		Size: int64(len(raw)),
		Mode: 0o644,
	}); err != nil {
		return fmt.Errorf("write export statistic header: %w", err)
	}
	if _, err := w.Write(raw); err != nil {
		return fmt.Errorf("write export statistic body: %w", err)
	}
	return nil
}
