package authz

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAuthorizerPermissionClosure(t *testing.T) {
	t.Parallel()

	authorizer, err := New(map[string][]string{
		"reader":        {string(AllRead), string(BackupWrite)},
		"s3-writer":     {string(S3Write)},
		"writer":        {string(AllWrite)},
		"file-writer":   {string(FileWrite)},
		"explicit":      {string(AdminRead)},
		"no-permission": {},
	})
	require.NoError(t, err)

	require.True(t, authorizer.Has("reader", S3Read))
	require.True(t, authorizer.Has("reader", AdminRead))
	require.True(t, authorizer.Has("reader", BackupWrite))
	require.False(t, authorizer.Has("reader", S3Write))
	require.True(t, authorizer.Has("s3-writer", S3Read))
	require.True(t, authorizer.Has("s3-writer", S3Write))
	require.False(t, authorizer.Has("s3-writer", WebDAVRead))
	require.True(t, authorizer.Has("writer", FileWrite))
	require.True(t, authorizer.Has("writer", AllRead))
	require.True(t, authorizer.Has("writer", AllWrite))
	require.True(t, authorizer.Has("file-writer", FileWrite))
	require.False(t, authorizer.Has("file-writer", S3Read))
	require.False(t, authorizer.Has("unknown", S3Read))
	require.False(t, authorizer.Has("reader", Permission("future:read")))
}

func TestAuthorizerLevelAndAny(t *testing.T) {
	t.Parallel()

	authorizer, err := New(map[string][]string{
		"reader": {string(WebDAVRead)},
		"writer": {string(WebDAVWrite)},
	})
	require.NoError(t, err)

	require.Equal(t, LevelRead, authorizer.Level("reader", WebDAVRead, WebDAVWrite))
	require.Equal(t, LevelReadWrite, authorizer.Level("writer", WebDAVRead, WebDAVWrite))
	require.Equal(t, LevelNone, authorizer.Level("missing", WebDAVRead, WebDAVWrite))
	require.True(t, authorizer.Any(WebDAVRead))
	require.True(t, authorizer.Any(WebDAVWrite))
	require.False(t, authorizer.Any(AdminRead))
}

func TestAuthorizerRejectsUnknownAndDuplicatePermissions(t *testing.T) {
	t.Parallel()

	for name, permissions := range map[string][]string{
		"unknown":   {"file:read"},
		"plain-all": {"all"},
		"duplicate": {string(S3Read), string(S3Read)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := New(map[string][]string{"user": permissions})
			require.Error(t, err)
			require.ErrorIs(t, err, ErrInvalidPermission)
		})
	}
}

func TestAuthorizerCopiesInput(t *testing.T) {
	t.Parallel()

	input := map[string][]string{"user": {string(S3Read)}}
	authorizer, err := New(input)
	require.NoError(t, err)
	input["user"][0] = string(AllWrite)
	input["other"] = []string{string(AllWrite)}

	require.True(t, authorizer.Has("user", S3Read))
	require.False(t, authorizer.Has("user", S3Write))
	require.False(t, authorizer.Has("other", S3Read))
}

func TestAuthorizerCompletePermissionMatrix(t *testing.T) {
	t.Parallel()

	allRead, err := New(map[string][]string{"user": {string(AllRead)}})
	require.NoError(t, err)
	allWrite, err := New(map[string][]string{"user": {string(AllWrite)}})
	require.NoError(t, err)

	for permission, class := range permissionClasses {
		require.True(t, allWrite.Has("user", permission), permission)
		require.Equal(t, class == classRead, allRead.Has("user", permission), permission)
	}
	for readPermission, writePermission := range writeForRead {
		authorizer, newErr := New(map[string][]string{
			"user": {string(writePermission)},
		})
		require.NoError(t, newErr)
		require.True(t, authorizer.Has("user", readPermission), readPermission)
		require.True(t, authorizer.Has("user", writePermission), writePermission)
	}
}

func TestAuthorizerRedundantPermissionsHaveEquivalentClosure(t *testing.T) {
	t.Parallel()

	redundant, err := New(map[string][]string{
		"user": {string(AllRead), string(S3Read), string(BackupWrite)},
	})
	require.NoError(t, err)
	minimal, err := New(map[string][]string{
		"user": {string(AllRead), string(BackupWrite)},
	})
	require.NoError(t, err)
	for permission := range permissionClasses {
		require.Equal(
			t,
			minimal.Has("user", permission),
			redundant.Has("user", permission),
			permission,
		)
	}
}

func TestAuthorizerSupportsConcurrentReads(t *testing.T) {
	t.Parallel()

	authorizer, err := New(map[string][]string{"user": {string(AllWrite)}})
	require.NoError(t, err)
	var workers sync.WaitGroup
	workers.Add(32)
	for range 32 {
		go func() {
			defer workers.Done()
			for range 1_000 {
				for permission := range permissionClasses {
					if !authorizer.Has("user", permission) {
						panic("immutable authorizer returned inconsistent result")
					}
				}
			}
		}()
	}
	workers.Wait()
}
