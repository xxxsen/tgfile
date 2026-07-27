package filemgr

import "io"

type cacheDirectoryLock interface {
	io.Closer
}
