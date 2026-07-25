package file

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExtractLinkFromFileKey(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		want    string
		wantErr bool
	}{
		{
			name: "canonical",
			key:  "0123456789abcdef-report.txt",
			want: "/defaults/01/0123456789abcdef-report.txt",
		},
		{
			name: "empty suffix",
			key:  "0123456789abcdef-",
			want: "/defaults/01/0123456789abcdef-",
		},
		{
			name: "utf8 suffix",
			key:  "0123456789abcdef-报告.pdf",
			want: "/defaults/01/0123456789abcdef-报告.pdf",
		},
		{name: "short", key: "a-x", wantErr: true},
		{name: "missing hash", key: "-xx", wantErr: true},
		{name: "uppercase hash", key: "0123456789abcdeF-file", wantErr: true},
		{name: "wrong separator", key: "0123456789abcdef_file", wantErr: true},
		{name: "slash", key: "0123456789abcdef-a/b", wantErr: true},
		{name: "backslash", key: `0123456789abcdef-a\b`, wantErr: true},
		{name: "control", key: "0123456789abcdef-a\nb", wantErr: true},
		{name: "too long", key: "0123456789abcdef-" + strings.Repeat("a", 129), wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ExtractLinkFromFileKey(test.key)
			if test.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, test.want, got)
		})
	}
}

func FuzzExtractLinkFromFileKey(f *testing.F) {
	for _, seed := range []string{
		"",
		"a-x",
		"0123456789abcdef-file",
		"0123456789abcdef-报告.pdf",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(_ *testing.T, value string) {
		_, _ = ExtractLinkFromFileKey(value)
	})
}
