package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"tgfile/filemgr"
	"tgfile/server/model"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/xxxsen/common/logutil"
	"go.uber.org/zap"
)

// Export 将s3数据导出
func Export(c *gin.Context) {
	ctx := c.Request.Context()
	c.Writer.Header().Set("Content-Encoding", "gzip")
	c.Writer.Header().Set("Content-Type", "application/tar+gzip")
	c.Writer.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=export.%d.tar.gz", time.Now().UnixMilli()))
	gz := gzip.NewWriter(c.Writer)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()
	st := &model.StatisticInfo{}
	start := time.Now()
	if err := filemgr.IterLink(ctx, "", func(ctx context.Context, link string, fileid uint64) (bool, error) {
		finfo, err := filemgr.Stat(ctx, fileid)
		if err != nil {
			return false, err
		}
		stream, err := filemgr.Open(ctx, fileid)
		if err != nil {
			return false, err
		}
		defer stream.Close()

		st.FileCount++
		st.FileSize += finfo.Size()
		h := &tar.Header{
			Name: link,
			Mode: 0644,
			Size: int64(finfo.Size()),
		}
		if err := tw.WriteHeader(h); err != nil {
			return false, fmt.Errorf("write header failed, fileid:%d, err:%w", fileid, err)
		}
		if _, err := io.Copy(tw, stream); err != nil {
			return false, fmt.Errorf("write body failed, fileid:%d, err:%w", fileid, err)
		}
		logutil.GetLogger(ctx).Debug("iter one link succ", zap.String("link", link), zap.Uint64("file_id", fileid))
		return true, nil
	}); err != nil {
		logutil.GetLogger(ctx).Error("iter link failed", zap.Error(err))
		return
	}
	cost := time.Since(start)
	st.TimeCost = cost.Milliseconds()
	if err := writeStatistic(tw, st); err != nil {
		logutil.GetLogger(ctx).Error("write export statistic info failed", zap.Error(err))
		return
	}
	logutil.GetLogger(ctx).Info("iter link and export succ")
}

func writeStatistic(w *tar.Writer, st *model.StatisticInfo) error {
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
