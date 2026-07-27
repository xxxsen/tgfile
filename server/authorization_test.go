package server_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xxxsen/tgfile/authz"
)

func testAuthorizer(t *testing.T, permissions map[string][]string) *authz.Authorizer {
	t.Helper()
	authorizer, err := authz.New(permissions)
	require.NoError(t, err)
	return authorizer
}
