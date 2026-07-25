package cachewrap

import (
	"context"
	"testing"

	"github.com/dgraph-io/ristretto/v2"
	"github.com/stretchr/testify/require"

	"github.com/xxxsen/tgfile/cacheapi"
)

func TestRistrettoSetIsImmediatelyVisible(t *testing.T) {
	cache, err := ristretto.NewCache(&ristretto.Config[uint64, []byte]{
		NumCounters:        10,
		MaxCost:            10,
		BufferItems:        64,
		IgnoreInternalCost: true,
		Cost: func(value []byte) int64 {
			return int64(len(value))
		},
	})
	require.NoError(t, err)
	wrapped := WrapRistrttoCache(cache)

	require.NoError(t, wrapped.Set(context.Background(), 1, []byte("value")))
	value, err := wrapped.Get(context.Background(), 1)
	require.NoError(t, err)
	require.Equal(t, []byte("value"), value)
}

func TestRistrettoSetReportsPolicyRejection(t *testing.T) {
	cache, err := ristretto.NewCache(&ristretto.Config[uint64, []byte]{
		NumCounters:        10,
		MaxCost:            1,
		BufferItems:        64,
		IgnoreInternalCost: true,
		Cost: func(value []byte) int64 {
			return int64(len(value))
		},
	})
	require.NoError(t, err)
	wrapped := WrapRistrttoCache(cache)

	err = wrapped.Set(context.Background(), 1, []byte("too large"))
	require.ErrorIs(t, err, cacheapi.ErrCacheSetRejected)
	_, err = wrapped.Get(context.Background(), 1)
	require.ErrorIs(t, err, cacheapi.ErrCacheKeyNotExist)
}
