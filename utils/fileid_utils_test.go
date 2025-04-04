package utils

import (
	"encoding/hex"
	"testing"
)

func TestFileidHash(t *testing.T) {
	for i := 0; i < 100; i++ {
		raw := FileIdToHash(uint64(i))
		t.Logf("index:%d => %s", i, hex.EncodeToString(raw))
	}
}
