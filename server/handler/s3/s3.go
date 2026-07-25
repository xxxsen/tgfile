package s3

import (
	"sync"

	"github.com/xxxsen/tgfile/filemgr"
)

type S3Handler struct {
	fmgr  filemgr.IFileManager
	locks *pathLocker
}

func NewS3Handler(fmgr filemgr.IFileManager) *S3Handler {
	return &S3Handler{fmgr: fmgr, locks: newPathLocker()}
}

type pathLockEntry struct {
	mutex sync.Mutex
	refs  int
}

type pathLocker struct {
	mutex sync.Mutex
	locks map[string]*pathLockEntry
}

func newPathLocker() *pathLocker {
	return &pathLocker{locks: make(map[string]*pathLockEntry)}
}

func (l *pathLocker) lock(key string) func() {
	l.mutex.Lock()
	entry := l.locks[key]
	if entry == nil {
		entry = &pathLockEntry{}
		l.locks[key] = entry
	}
	entry.refs++
	l.mutex.Unlock()

	entry.mutex.Lock()
	return func() {
		entry.mutex.Unlock()
		l.mutex.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(l.locks, key)
		}
		l.mutex.Unlock()
	}
}
