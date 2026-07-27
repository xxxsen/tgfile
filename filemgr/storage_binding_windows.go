//go:build windows

package filemgr

import (
	"encoding/binary"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func cacheDatabaseFileIdentity(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open database for cache binding: %w", err)
	}
	defer func() { _ = file.Close() }()
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &info); err != nil {
		return nil, fmt.Errorf("read database file identity: %w", err)
	}
	var result [12]byte
	binary.BigEndian.PutUint32(result[:4], info.VolumeSerialNumber)
	binary.BigEndian.PutUint32(result[4:8], info.FileIndexHigh)
	binary.BigEndian.PutUint32(result[8:12], info.FileIndexLow)
	return result[:], nil
}
