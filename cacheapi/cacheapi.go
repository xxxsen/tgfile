package cacheapi

import (
	"context"
	"errors"
)

var (
	ErrCacheKeyNotExist = errors.New("cache key not exist")
)

type ICacheGetter[K comparable, V any] interface {
	Get(ctx context.Context, k K) (V, error)
}

type ICacheSetter[K comparable, V any] interface {
	Set(ctx context.Context, k K, v V) error
}

type ICacheDeleter[K comparable] interface {
	Del(ctx context.Context, k K) error
}

type ICache[K comparable, V any] interface {
	ICacheLoader[K, V]
	ICacheDeleter[K]
}

type ICacheLoader[K comparable, V any] interface {
	ICacheGetter[K, V]
	ICacheSetter[K, V]
}

type LoadCacheCallbackFunc[K comparable, V any] func(ctx context.Context, miss []K) (map[K]V, error)

func Load[K comparable, V any](ctx context.Context, c ICacheLoader[K, V], k K, cb LoadCacheCallbackFunc[K, V]) (V, error) {
	var defaultV V
	rs, err := LoadMany(ctx, c, []K{k}, cb)
	if err != nil {
		return defaultV, err
	}
	v, ok := rs[k]
	if !ok {
		return defaultV, nil
	}
	return v, nil
}

func LoadMany[K comparable, V any](ctx context.Context, c ICacheLoader[K, V], ks []K, cb LoadCacheCallbackFunc[K, V]) (map[K]V, error) {
	m := make(map[K]V, len(ks))
	miss := make([]K, 0, len(ks))
	for _, k := range ks {
		v, err := c.Get(ctx, k)
		if err != nil {
			if errors.Is(err, ErrCacheKeyNotExist) {
				miss = append(miss, k)
				continue
			}
			return nil, err
		}
		m[k] = v
	}
	if len(miss) == 0 {
		return m, nil
	}
	rs, err := cb(ctx, miss)
	if err != nil {
		return nil, err
	}
	for k, v := range rs {
		m[k] = v
		_ = c.Set(ctx, k, v)
	}
	return m, nil
}
