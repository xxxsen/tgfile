package authz

import (
	"errors"
	"fmt"
)

type Permission string

const (
	S3Read      Permission = "s3:read"
	S3Write     Permission = "s3:write"
	WebDAVRead  Permission = "webdav:read"
	WebDAVWrite Permission = "webdav:write"
	BackupRead  Permission = "backup:read"
	BackupWrite Permission = "backup:write"
	AdminRead   Permission = "admin:read"
	AdminWrite  Permission = "admin:write"
	FileWrite   Permission = "file:write"
	AllRead     Permission = "all:read"
	AllWrite    Permission = "all:write"
)

type Level uint8

const (
	LevelNone Level = iota
	LevelRead
	LevelReadWrite
)

var ErrInvalidPermission = errors.New("invalid permission")

type permissionClass uint8

const (
	classRead permissionClass = iota + 1
	classWrite
)

var permissionClasses = map[Permission]permissionClass{
	S3Read:      classRead,
	S3Write:     classWrite,
	WebDAVRead:  classRead,
	WebDAVWrite: classWrite,
	BackupRead:  classRead,
	BackupWrite: classWrite,
	AdminRead:   classRead,
	AdminWrite:  classWrite,
	FileWrite:   classWrite,
	AllRead:     classRead,
	AllWrite:    classWrite,
}

var writeForRead = map[Permission]Permission{
	S3Read:     S3Write,
	WebDAVRead: WebDAVWrite,
	BackupRead: BackupWrite,
	AdminRead:  AdminWrite,
}

type Authorizer struct {
	grants map[string]map[Permission]struct{}
}

func New(input map[string][]string) (*Authorizer, error) {
	grants := make(map[string]map[Permission]struct{}, len(input))
	for username, values := range input {
		userGrants := make(map[Permission]struct{}, len(values))
		for _, value := range values {
			permission := Permission(value)
			if _, exists := permissionClasses[permission]; !exists {
				return nil, fmt.Errorf(
					"%w %q for user %q",
					ErrInvalidPermission,
					value,
					username,
				)
			}
			if _, duplicate := userGrants[permission]; duplicate {
				return nil, fmt.Errorf(
					"%w: duplicate %q for user %q",
					ErrInvalidPermission,
					value,
					username,
				)
			}
			userGrants[permission] = struct{}{}
		}
		grants[username] = userGrants
	}
	return &Authorizer{grants: grants}, nil
}

func (a *Authorizer) Has(username string, permission Permission) bool {
	class, known := permissionClasses[permission]
	if a == nil || !known {
		return false
	}
	grants, exists := a.grants[username]
	if !exists {
		return false
	}
	if _, exists := grants[AllWrite]; exists {
		return true
	}
	if _, exists := grants[permission]; exists {
		return true
	}
	if class == classRead {
		if _, exists := grants[AllRead]; exists {
			return true
		}
		if write, exists := writeForRead[permission]; exists {
			_, exists = grants[write]
			return exists
		}
	}
	return false
}

func (a *Authorizer) Any(permission Permission) bool {
	if a == nil {
		return false
	}
	for username := range a.grants {
		if a.Has(username, permission) {
			return true
		}
	}
	return false
}

func (a *Authorizer) Level(
	username string,
	readPermission Permission,
	writePermission Permission,
) Level {
	if a.Has(username, writePermission) {
		return LevelReadWrite
	}
	if a.Has(username, readPermission) {
		return LevelRead
	}
	return LevelNone
}
