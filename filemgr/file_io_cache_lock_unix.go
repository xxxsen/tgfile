//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package filemgr

import (
	"errors"
	"fmt"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

type unixCacheDirectoryLock struct {
	file       *os.File
	descriptor int
}

func acquireCacheDirectoryLock(path string) (cacheDirectoryLock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open L2 directory lock: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("secure L2 directory lock: %w", err)
	}
	descriptor, err := checkedFileDescriptor(file)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := unix.Flock(descriptor, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, ErrCacheDirInUse
		}
		return nil, fmt.Errorf("lock L2 directory: %w", err)
	}
	return &unixCacheDirectoryLock{file: file, descriptor: descriptor}, nil
}

func (l *unixCacheDirectoryLock) Close() error {
	return errors.Join(unix.Flock(l.descriptor, unix.LOCK_UN), l.file.Close())
}

func checkedFileDescriptor(file *os.File) (int, error) {
	descriptor := file.Fd()
	converted, err := strconv.Atoi(strconv.FormatUint(uint64(descriptor), 10))
	if err != nil {
		return 0, fmt.Errorf("%w: parse lock file descriptor: %w", ErrInvalidCachePath, err)
	}
	return converted, nil
}
