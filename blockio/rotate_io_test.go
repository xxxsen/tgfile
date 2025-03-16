package blockio

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
)

type fakeIO struct {
	data []byte
}

func (f *fakeIO) MaxFileSize() int64 {
	return 1024 * 1024 * 1024
}

func (f *fakeIO) Upload(ctx context.Context, r io.Reader) (string, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	f.data = raw
	return "test", nil
}

func (f *fakeIO) Download(ctx context.Context, filekey string, pos int64) (io.ReadCloser, error) {
	if filekey != "test" {
		return nil, fmt.Errorf("key:%s not found", filekey)
	}
	data := f.data
	if int(pos) > len(data) {
		return nil, fmt.Errorf("pos overflow, pos:%d", pos)
	}
	data = data[pos:]
	return io.NopCloser(bytes.NewReader(data)), nil
}

func TestRotateIO(t *testing.T) {
	const maxBytes = 100000
	fakeio := &fakeIO{}
	for rotateVal := 0; rotateVal < 258; rotateVal++ {
		stream := NewRotateIO(fakeio, rotateVal)
		ctx := context.Background()
		data := bytes.NewBuffer(nil)
		rnd := rand.Int()
		for i := rnd; i < rnd+maxBytes; i++ {
			_ = data.WriteByte(byte(i))
		}
		raw := data.Bytes()
		key, err := stream.Upload(ctx, bytes.NewReader(raw))
		assert.NoError(t, err)
		if rotateVal%256 != 0 {
			assert.NotEqual(t, raw, fakeio.data)
		} else {
			assert.Equal(t, raw, fakeio.data)
		}
		for i := 0; i < 10; i++ {
			randpos := int64(rand.Int() % maxBytes)
			rc, err := stream.Download(ctx, key, randpos)
			assert.NoError(t, err)
			down, err := io.ReadAll(rc)
			assert.NoError(t, err)
			_ = rc.Close()
			assert.Equal(t, raw[randpos:], down)
		}
	}
}
