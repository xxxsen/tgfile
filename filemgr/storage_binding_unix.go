//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package filemgr

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"syscall"
)

func cacheDatabaseFileIdentity(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat database for cache binding: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, fmt.Errorf("read database file identity: %w", ErrInvalidCachePath)
	}
	var result bytes.Buffer
	if err := binary.Write(&result, binary.BigEndian, stat.Dev); err != nil {
		return nil, fmt.Errorf("encode database device identity: %w", err)
	}
	if err := binary.Write(&result, binary.BigEndian, stat.Ino); err != nil {
		return nil, fmt.Errorf("encode database inode identity: %w", err)
	}
	return result.Bytes(), nil
}
