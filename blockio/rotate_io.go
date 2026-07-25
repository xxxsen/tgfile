package blockio

import (
	"context"
	"fmt"
	"io"
)

type rotateIO struct {
	impl      IBlockIO
	rotateVal int
}

func NewRotateIO(impl IBlockIO, rotateVal int) IBlockIO {
	if rotateVal <= 0 {
		return impl
	}
	return &rotateIO{impl: impl, rotateVal: rotateVal}
}

func (r *rotateIO) Name() string {
	return r.impl.Name()
}

func (r *rotateIO) MaxFileSize() int64 {
	return r.impl.MaxFileSize()
}

func (r *rotateIO) Upload(ctx context.Context, reader io.Reader) (string, error) {
	reader = newRotateReadCloser(io.NopCloser(reader), r.rotateVal)
	key, err := r.impl.Upload(ctx, reader)
	if err != nil {
		return "", fmt.Errorf("upload rotated block: %w", err)
	}
	return key, nil
}

func (r *rotateIO) Download(ctx context.Context, filekey string, pos int64) (io.ReadCloser, error) {
	rc, err := r.impl.Download(ctx, filekey, pos)
	if err != nil {
		return nil, fmt.Errorf("download rotated block: %w", err)
	}
	rc = newRotateReadCloser(rc, -r.rotateVal)
	return rc, nil
}

type rotateReadCloser struct {
	rc  io.ReadCloser
	val int
}

func newRotateReadCloser(rc io.ReadCloser, val int) io.ReadCloser {
	val %= 256
	if val < 0 {
		val += 256
	}
	return rotateReadCloser{
		rc:  rc,
		val: val,
	}
}

func (r rotateReadCloser) Read(p []byte) (int, error) {
	n, err := r.rc.Read(p)
	if n > 0 {
		for i := 0; i < n; i++ {
			p[i] = byte((int(p[i]) + r.val) % 256) //nolint:gosec // Expression is normalized to [0, 255].
		}
	}
	if err != nil && err != io.EOF {
		return n, fmt.Errorf("read rotated block: %w", err)
	}
	if err == io.EOF {
		return n, io.EOF
	}
	return n, nil
}

func (r rotateReadCloser) Close() error {
	if err := r.rc.Close(); err != nil {
		return fmt.Errorf("close rotated block: %w", err)
	}
	return nil
}
