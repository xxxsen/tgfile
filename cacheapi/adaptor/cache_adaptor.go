package cachewrap

import (
	"context"

	lru "github.com/hashicorp/golang-lru/v2"
	explru "github.com/hashicorp/golang-lru/v2/expirable"
	"github.com/xxxsen/tgfile/cacheapi"
)

type lruCacheAdaptor[K comparable, V any] struct {
	c *lru.Cache[K, V]
}

func (l *lruCacheAdaptor[K, V]) Get(ctx context.Context, k K) (V, error) {
	v, ok := l.c.Get(k)
	if !ok {
		return v, cacheapi.ErrCacheKeyNotExist
	}
	return v, nil
}

func (l *lruCacheAdaptor[K, V]) Set(ctx context.Context, k K, v V) error {
	_ = l.c.Add(k, v)
	return nil
}

func (l *lruCacheAdaptor[K, V]) Del(ctx context.Context, k K) error {
	_ = l.c.Remove(k)
	return nil
}

func WrapLruCache[K comparable, V any](in *lru.Cache[K, V]) cacheapi.ICache[K, V] {
	return &lruCacheAdaptor[K, V]{
		c: in,
	}
}

type expirableLruCacheAdaptor[K comparable, V any] struct {
	c *explru.LRU[K, V]
}

func (e *expirableLruCacheAdaptor[K, V]) Get(ctx context.Context, k K) (V, error) {
	v, ok := e.c.Get(k)
	if !ok {
		return v, cacheapi.ErrCacheKeyNotExist
	}
	return v, nil
}

func (e *expirableLruCacheAdaptor[K, V]) Set(ctx context.Context, k K, v V) error {
	_ = e.c.Add(k, v)
	return nil
}

func (e *expirableLruCacheAdaptor[K, V]) Del(ctx context.Context, k K) error {
	_ = e.c.Remove(k)
	return nil
}

func WrapExpirableLruCache[K comparable, V any](in *explru.LRU[K, V]) cacheapi.ICache[K, V] {
	return &expirableLruCacheAdaptor[K, V]{
		c: in,
	}
}
