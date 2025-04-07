package cacheapi

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

type simpleCache[K comparable, V any] struct {
	m map[K]V
}

func (s *simpleCache[K, V]) Get(ctx context.Context, k K) (V, error) {
	v, ok := s.m[k]
	if !ok {
		return v, ErrCacheKeyNotExist
	}
	return v, nil
}

func (s *simpleCache[K, V]) Set(ctx context.Context, k K, v V) error {
	s.m[k] = v
	return nil
}

func newSimpleCache[K comparable, V any]() ICacheLoader[K, V] {
	return &simpleCache[K, V]{m: map[K]V{}}
}

func TestLoad(t *testing.T) {
	c := newSimpleCache[int, string]()
	ctx := context.Background()
	v, err := Load(ctx, c, 1, func(ctx context.Context, miss []int) (map[int]string, error) {
		rs := make(map[int]string, len(miss))
		for _, k := range miss {
			rs[k] = fmt.Sprintf("%d", k)
		}
		return rs, nil
	})
	assert.NoError(t, err)
	assert.Equal(t, "1", v)
	v, err = c.Get(ctx, 1)
	assert.NoError(t, err)
	assert.Equal(t, "1", v)
	_, err = c.Get(ctx, 2)
	assert.Error(t, err)
}

func TestLoadMany(t *testing.T) {
	c := newSimpleCache[int, string]()
	ctx := context.Background()
	testList := []int{1, 2, 3}
	rs, err := LoadMany(ctx, c, testList, func(ctx context.Context, miss []int) (map[int]string, error) {
		rs := make(map[int]string, len(miss))
		for _, k := range miss {
			rs[k] = fmt.Sprintf("%d", k)
		}
		return rs, nil
	})
	assert.NoError(t, err)
	assert.Equal(t, len(testList), len(rs))
	for _, k := range testList {
		v, err := c.Get(ctx, k)
		assert.NoError(t, err)
		assert.Equal(t, fmt.Sprintf("%d", k), v)
	}
}
