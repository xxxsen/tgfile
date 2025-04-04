package utils

import (
	"fmt"
	"io"
	"os"
	"path"

	"github.com/google/uuid"
)

func SafeSaveIOToFile(dst string, r io.Reader) error {
	// 基于dst添加一个.temp后缀生成临时文件, 将io写到这个临时文件, 然后再通过move操作覆盖目标文件
	// 这里简单实现, 具体逻辑可以根据实际需求来
	dir := path.Dir(dst)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create directory failed: %w", err)
	}
	dstTmp := dst + "." + uuid.NewString() + ".temp"
	f, err := os.OpenFile(dstTmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("create tmp file failed: %w", err)
	}
	defer os.Remove(dstTmp)
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		return fmt.Errorf("copy stream to tmp file failed: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close tmp file failed: %w", err)
	}
	// 替换目标文件
	if err := os.Rename(dstTmp, dst); err != nil {
		return fmt.Errorf("rename tmp file to target failed: %w", err)
	}
	return nil
}
