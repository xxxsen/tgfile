//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris && !windows

package filemgr

func acquireCacheDirectoryLock(string) (cacheDirectoryLock, error) {
	return nil, ErrCacheLockUnsupported
}
