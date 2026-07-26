//go:build windows

package backupmgr

import (
	"fmt"

	"golang.org/x/sys/windows"
)

func filesystemFreeBytes(workDir string) (int64, error) {
	directoryName, err := windows.UTF16PtrFromString(workDir)
	if err != nil {
		return 0, fmt.Errorf("encode filesystem path: %w", err)
	}
	var available uint64
	if err := windows.GetDiskFreeSpaceEx(directoryName, &available, nil, nil); err != nil {
		return 0, fmt.Errorf("query filesystem space: %w", err)
	}
	return saturatingFilesystemBytes(available, 1), nil
}
