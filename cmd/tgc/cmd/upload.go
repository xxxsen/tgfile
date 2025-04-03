package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"github.com/xxxsen/common/logutil"
	"go.uber.org/zap"
)

type uploadArgs struct {
	file string
}

func NewUploadCmd(c *Context) *cobra.Command {
	args := &uploadArgs{}
	ctx := context.Background()
	subc := &cobra.Command{
		Use:   "upload",
		Short: "Upload a file",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return onRunUpload(ctx, c, args)
		},
	}
	subc.PersistentFlags().StringVarP(&args.file, "file", "f", "", "local file to upload")
	return subc
}

func onRunUpload(ctx context.Context, c *Context, args *uploadArgs) error {
	if len(args.file) == 0 {
		return fmt.Errorf("no upload file found")
	}
	start := time.Now()
	filekey, err := c.TGC.UploadFile(context.Background(), args.file)
	if err != nil {
		return fmt.Errorf("upload file failed, err:%w", err)
	}
	link := fmt.Sprintf("%s://%s/file/download/%s", c.Config.Schema, c.Config.Host, filekey)
	logutil.GetLogger(ctx).Info("upload file succ", zap.String("link", link), zap.Duration("cost", time.Since(start)))
	return nil
}

func init() {
	register(NewUploadCmd)
}
