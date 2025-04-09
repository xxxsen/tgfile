package cachewrap

import (
	"context"

	"github.com/dgraph-io/ristretto/v2"
	"github.com/xxxsen/tgfile/cacheapi"
)

type LimitRistrettoKey interface {
	uint64 | string | byte | int | int32 | uint32 | int64
}

type ristrettoCacheWrap[K LimitRistrettoKey, V any] struct {
	c *ristretto.Cache[K, V]
}

func (r *ristrettoCacheWrap[K, V]) Get(ctx context.Context, k K) (V, error) {
	v, ok := r.c.Get(k)
	if !ok {
		return v, cacheapi.ErrCacheKeyNotExist
	}
	return v, nil
}

func (r *ristrettoCacheWrap[K, V]) Set(ctx context.Context, k K, v V) error {
	_ = r.c.Set(k, v, 0)
	return nil
}

func (r *ristrettoCacheWrap[K, V]) Del(ctx context.Context, k K) error {
	r.c.Del(k)
	return nil
}

func WrapRistrttoCache[K LimitRistrettoKey, V any](c *ristretto.Cache[K, V]) cacheapi.ICache[K, V] {
	return &ristrettoCacheWrap[K, V]{c: c}
}
