//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris && !windows

package filemgr

import "github.com/google/uuid"

var cacheProcessIdentity = []byte(uuid.NewString())

func cacheDatabaseFileIdentity(string) ([]byte, error) {
	return append([]byte(nil), cacheProcessIdentity...), nil
}
