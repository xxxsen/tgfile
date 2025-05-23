package backup

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
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
	c.Writer.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=export.%d.tar.gz", time.Now().UnixMilli()))
	gz := gzip.NewWriter(c.Writer)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()
	st := &model.StatisticInfo{}
	start := time.Now()
	if err := fs.WalkDir(filemgr.ToFileSystem(ctx, h.fmgr), "/", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		ent, err := h.fmgr.StatFileLink(ctx, path)
		if err != nil {
			return err
		}
		stream, err := h.fmgr.OpenFile(ctx, ent.FileId)
		if err != nil {
			return err
		}
		defer stream.Close()

		st.FileCount++
		st.FileSize += ent.FileSize
		h := &tar.Header{
			Name: path,
			Mode: int64(ent.Mode),
			Size: int64(ent.FileSize),
		}
		if err := tw.WriteHeader(h); err != nil {
			return fmt.Errorf("write header failed, fileid:%d, err:%w", ent.FileId, err)
		}
		if _, err := io.Copy(tw, stream); err != nil {
			return fmt.Errorf("write body failed, fileid:%d, err:%w", ent.FileId, err)
		}
		logutil.GetLogger(ctx).Debug("iter one link succ", zap.String("link", path), zap.Uint64("file_id", ent.FileId))
		return nil
	}); err != nil {
		logutil.GetLogger(ctx).Error("iter link failed", zap.Error(err))
		return
	}
	cost := time.Since(start)
	st.TimeCost = cost.Milliseconds()
	if err := h.writeStatistic(tw, st); err != nil {
		logutil.GetLogger(ctx).Error("write export statistic info failed", zap.Error(err))
		return
	}
	logutil.GetLogger(ctx).Info("iter link and export succ")
}

func (h *BackupHandler) writeStatistic(w *tar.Writer, st *model.StatisticInfo) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if err := w.WriteHeader(&tar.Header{
		Name: defaultStatisticFileName,
		Size: int64(len(raw)),
		Mode: 0644,
	}); err != nil {
		return err
	}
	if _, err := w.Write(raw); err != nil {
		return err
	}
	return nil
}
