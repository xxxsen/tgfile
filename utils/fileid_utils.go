package utils

import (
	"encoding/binary"
	"encoding/hex"

	"github.com/cespare/xxhash/v2"
)

func EncodeFileId(fileid uint64) string {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, fileid)
	return hex.EncodeToString(buf)
}

func DecodeFileId(xfid string) (uint64, error) {
	raw, err := hex.DecodeString(xfid)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(raw), nil
}

func FileIdToHash(fid uint64) []byte {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, fid)
	v := xxhash.Sum64(buf)
	binary.BigEndian.PutUint64(buf, v)
	return buf
}
