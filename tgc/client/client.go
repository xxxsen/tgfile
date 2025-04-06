package client

import (
	"context"
	"io"
)

type IClient interface {
	CreateDraft(ctx context.Context, filename string, size int64) (string, int64, error)
	CreatePart(ctx context.Context, uploadKey string, partid int64, r io.Reader) error
	FinishCreate(ctx context.Context, uploadKey string) (string, error)
}
