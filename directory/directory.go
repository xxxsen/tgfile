package directory

import (
	"context"

	"github.com/xxxsen/common/database"
)

type DirectoryScanCallbackFunc func(ctx context.Context, res []IDirectoryEntry) (bool, error)

type IDirectoryEntryIdentity interface {
	EntryID() uint64
	RefData() string
	Name() string
	IsDir() bool
}

type IDirectoryEntryMetadata interface {
	Ctime() int64
	Mtime() int64
	Mode() uint32
	Size() int64
}

type IDirectoryEntry interface {
	IDirectoryEntryIdentity
	IDirectoryEntryMetadata
}

type IDirectoryWriter interface {
	Mkdir(ctx context.Context, dir string) error
	Copy(ctx context.Context, src, dst string, overwrite bool) error
	Move(ctx context.Context, src, dst string, overwrite bool) error
	Create(ctx context.Context, filename string, size int64, refdata string) error
	Remove(ctx context.Context, filename string) error
}

type IDirectoryReader interface {
	List(ctx context.Context, dir string) ([]IDirectoryEntry, error)
	Iterate(ctx context.Context, dir string, batch uint, cb DirectoryScanCallbackFunc) error
	Stat(ctx context.Context, filename string) (IDirectoryEntry, error)
	Scan(ctx context.Context, batch uint, cb DirectoryScanCallbackFunc) error
}

type IDirectory interface {
	IDirectoryWriter
	IDirectoryReader
}

type ITransactionReader interface {
	Stat(ctx context.Context, filename string) (IDirectoryEntry, bool, error)
}

type ITransactionMutation interface {
	Create(ctx context.Context, filename string, size int64, refdata string) (IDirectoryEntry, error)
	Mkdir(ctx context.Context, dirname string) (IDirectoryEntry, error)
	Replace(
		ctx context.Context,
		filename string,
		size int64,
		refdata string,
		mtime int64,
	) (IDirectoryEntry, error)
	Remove(ctx context.Context, filename string) ([]IDirectoryEntry, error)
	Touch(ctx context.Context, filename string, mtime int64) error
}

type ITransactionTransfer interface {
	Copy(ctx context.Context, source, destination string, overwrite bool) ([]EntryCopy, []IDirectoryEntry, error)
	CopyDepth(
		ctx context.Context,
		source, destination string,
		overwrite, recursive bool,
	) ([]EntryCopy, []IDirectoryEntry, error)
	Move(ctx context.Context, source, destination string, overwrite bool) ([]IDirectoryEntry, error)
}

type ITransaction interface {
	ITransactionReader
	ITransactionMutation
	ITransactionTransfer
	QueryExecer() database.IQueryExecer
}

type EntryCopy struct {
	Source      IDirectoryEntry
	Destination IDirectoryEntry
}

type TransactionFunc func(ctx context.Context, tx ITransaction) error

type ITransactionalDirectory interface {
	IDirectory
	WithTransaction(ctx context.Context, callback TransactionFunc) error
}
