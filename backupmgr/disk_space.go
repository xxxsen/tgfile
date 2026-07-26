package backupmgr

import (
	"math"
	"math/big"
)

func saturatingFilesystemBytes(blocks, blockSize uint64) int64 {
	if blocks == 0 || blockSize == 0 {
		return 0
	}
	product := new(big.Int).SetUint64(blocks)
	product.Mul(product, new(big.Int).SetUint64(blockSize))
	if !product.IsInt64() {
		return math.MaxInt64
	}
	return product.Int64()
}
