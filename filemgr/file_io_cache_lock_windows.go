//go:build windows

package filemgr

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

type windowsCacheDirectoryLock struct {
	file       *os.File
	overlapped windows.Overlapped
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
	lock := &windowsCacheDirectoryLock{file: file}
	err = windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		1,
		0,
		&lock.overlapped,
	)
	if err != nil {
		_ = file.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, ErrCacheDirInUse
		}
		return nil, fmt.Errorf("lock L2 directory: %w", err)
	}
	return lock, nil
}

func (l *windowsCacheDirectoryLock) Close() error {
	unlockErr := windows.UnlockFileEx(
		windows.Handle(l.file.Fd()),
		0,
		1,
		0,
		&l.overlapped,
	)
	return errors.Join(unlockErr, l.file.Close())
}
