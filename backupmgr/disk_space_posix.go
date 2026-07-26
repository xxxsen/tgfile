//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package backupmgr

import (
	"fmt"
	"syscall"
)

func filesystemFreeBytes(workDir string) (int64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(workDir, &stat); err != nil {
		return 0, fmt.Errorf("stat filesystem: %w", err)
	}
	if stat.Bsize <= 0 {
		return 0, syscall.EINVAL
	}
	return saturatingFilesystemBytes(stat.Bavail, uint64(stat.Bsize)), nil
}
