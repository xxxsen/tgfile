package blockio

import (
	"bytes"
	"context"
	"encoding/hex"
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
	const maxBytes = 1000
	fakeio := &fakeIO{}
	stream := NewRotateIO(fakeio, 123)
	ctx := context.Background()
	data := bytes.NewBuffer(nil)
	for i := 0; i < maxBytes; i++ {
		_ = data.WriteByte(byte(i))
	}
	raw := data.Bytes()
	key, err := stream.Upload(ctx, bytes.NewReader(raw))
	assert.NoError(t, err)
	assert.NotEqual(t, raw, fakeio.data)
	for i := 0; i < 10; i++ {
		randpos := int64(rand.Int() % maxBytes)
		rc, err := stream.Download(ctx, key, randpos)
		assert.NoError(t, err)
		down, err := io.ReadAll(rc)
		assert.NoError(t, err)
		_ = rc.Close()
		t.Logf("read data:%s", hex.EncodeToString(down))
		assert.Equal(t, raw[randpos:], down)
	}
}
