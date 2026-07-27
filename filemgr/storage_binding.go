package filemgr

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
)

const storageBindingFormatVersion = uint32(1)

func BuildStorageBinding(
	databasePath, backendKind string,
	backendConfig any,
	rotateStream int,
) ([sha256.Size]byte, error) {
	absolute, err := filepath.Abs(databasePath)
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("resolve database path for cache binding: %w", err)
	}
	canonical := filepath.Clean(absolute)
	if resolved, err := filepath.EvalSymlinks(canonical); err == nil {
		canonical = resolved
	}
	fileIdentity, err := cacheDatabaseFileIdentity(canonical)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	backendJSON, err := json.Marshal(backendConfig)
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("encode backend configuration for cache binding: %w", err)
	}
	hash := sha256.New()
	writeBindingField(hash, uint64(storageBindingFormatVersion), nil)
	writeBindingField(hash, uint64(len(canonical)), []byte(canonical))
	writeBindingField(hash, uint64(len(fileIdentity)), fileIdentity)
	writeBindingField(hash, uint64(len(backendKind)), []byte(backendKind))
	writeBindingField(hash, uint64(len(backendJSON)), backendJSON)
	_ = binary.Write(hash, binary.BigEndian, int64(rotateStream))
	var binding [sha256.Size]byte
	copy(binding[:], hash.Sum(nil))
	return binding, nil
}

func writeBindingField(writer io.Writer, value uint64, raw []byte) {
	var buffer [8]byte
	binary.BigEndian.PutUint64(buffer[:], value)
	_, _ = writer.Write(buffer[:])
	_, _ = writer.Write(raw)
}
