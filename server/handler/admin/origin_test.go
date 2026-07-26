package admin

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateAdminHandlerOrigins(t *testing.T) {
	origins, secure, err := validateAdminHandlerOrigins([]string{
		"https://IMAGE.example.test:443/",
		"https://files.example.test",
	})
	require.NoError(t, err)
	require.True(t, secure)
	require.Equal(t, map[string]struct{}{
		"https://image.example.test": {},
		"https://files.example.test": {},
	}, origins)

	for _, test := range []struct {
		name    string
		origins []string
	}{
		{
			name:    "non-loopback HTTP",
			origins: []string{"http://image.example.test"},
		},
		{
			name:    "mixed schemes",
			origins: []string{"https://image.example.test", "http://localhost"},
		},
		{
			name: "duplicate canonical origin",
			origins: []string{
				"https://image.example.test",
				"https://IMAGE.example.test:443/",
			},
		},
		{
			name:    "path is rejected",
			origins: []string{"https://image.example.test/admin"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := validateAdminHandlerOrigins(test.origins)
			require.Error(t, err)
		})
	}
}
