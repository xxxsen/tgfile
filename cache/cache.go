package cache

import (
	"context"
	"time"

	lru "github.com/hnlq715/golang-lru"
)

var defaultInst ICache

type LoadFunc func(ctx context.Context, c ICache, k string) (interface{}, bool, error)
type LoadManyFunc func(ctx context.Context, c ICache, ks []string) (map[string]interface{}, error)

type ICache interface {
	Set(ctx context.Context, key string, v interface{}, expire time.Duration) error
	Get(ctx context.Context, key string) (interface{}, bool, error)
	Del(ctx context.Context, key string) error
}

func SetImpl(c ICache) {
	defaultInst = c
}

type cacheImpl struct {
	c *lru.Cache
}

func (c *cacheImpl) Set(ctx context.Context, k string, v interface{}, expire time.Duration) error {
	c.c.AddEx(k, v, expire)
	return nil
}

func (c *cacheImpl) Get(ctx context.Context, k string) (interface{}, bool, error) {
	v, ok := c.c.Get(k)
	return v, ok, nil
}

func (c *cacheImpl) Del(ctx context.Context, k string) error {
	c.c.Remove(k)
	return nil
}

func New(sz int) (ICache, error) {
	c, err := lru.New(sz)
	if err != nil {
		return nil, err
	}
	return &cacheImpl{
		c: c,
	}, nil
}

func MustNew(sz int) ICache {
	c, err := New(sz)
	if err != nil {
		panic(err)
	}
	return c
}

func Set(ctx context.Context, k string, v interface{}, expire time.Duration) error {
	return defaultInst.Set(ctx, k, v, expire)
}

func Get(ctx context.Context, k string) (interface{}, bool, error) {
	return defaultInst.Get(ctx, k)
}

func Del(ctx context.Context, k string) error {
	return defaultInst.Del(ctx, k)
}

func Load(ctx context.Context, k string, fn LoadFunc) (interface{}, bool, error) {
	v, ok, err := Get(ctx, k)
	if err != nil {
		return nil, false, err
	}
	if ok {
		return v, true, nil
	}
	v, ok, err = fn(ctx, defaultInst, k)
	if err != nil {
		return nil, false, err
	}
	return v, ok, nil
}

func LoadMany(ctx context.Context, ks []string, fn LoadManyFunc) (map[string]interface{}, error) {
	rs := make(map[string]interface{}, len(ks))
	miss := make([]string, 0, len(ks))
	for _, k := range ks {
		v, ok, err := Get(ctx, k)
		if err != nil {
			return nil, err
		}
		if !ok {
			miss = append(miss, k)
			continue
		}
		rs[k] = v
	}
	if len(miss) == 0 {
		return rs, nil
	}
	loaddata, err := fn(ctx, defaultInst, miss)
	if err != nil {
		return nil, err
	}
	for k, v := range loaddata {
		rs[k] = v
	}
	return rs, nil
}
