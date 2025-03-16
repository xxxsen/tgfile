package blockio

import (
	"context"
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

func (r *rotateIO) MaxFileSize() int64 {
	return r.impl.MaxFileSize()
}

func (rt *rotateIO) Upload(ctx context.Context, r io.Reader) (string, error) {
	r = newRotateReadCloser(io.NopCloser(r), rt.rotateVal)
	return rt.impl.Upload(ctx, r)
}

func (rt *rotateIO) Download(ctx context.Context, filekey string, pos int64) (io.ReadCloser, error) {
	rc, err := rt.impl.Download(ctx, filekey, pos)
	if err != nil {
		return nil, err
	}
	rc = newRotateReadCloser(rc, -1*rt.rotateVal)
	return rc, nil
}

type rotateReadCloser struct {
	rc  io.ReadCloser
	val int
}

func newRotateReadCloser(rc io.ReadCloser, val int) io.ReadCloser {
	return rotateReadCloser{
		rc:  rc,
		val: val,
	}
}

func (r rotateReadCloser) Read(p []byte) (int, error) {
	n, err := r.rc.Read(p)
	if n > 0 {
		for i := 0; i < n; i++ {
			p[i] = uint8((int(p[i]) + r.val) % 256)
		}
	}
	return n, err
}

func (r rotateReadCloser) Close() error {
	return r.rc.Close()
}
