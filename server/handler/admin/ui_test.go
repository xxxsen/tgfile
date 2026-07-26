package admin

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xxxsen/tgfile/server/handler/admin/ui"
)

func TestEmbeddedAdminUIHasNoInlineOrRemoteExecutableContent(t *testing.T) {
	html, err := ui.Read("index.html")
	require.NoError(t, err)
	javascript, err := ui.Read("app.js")
	require.NoError(t, err)
	styles, err := ui.Read("styles.css")
	require.NoError(t, err)
	require.NotEmpty(t, styles)

	page := string(html)
	require.Contains(t, page, `src="/_admin/assets/app.js"`)
	require.Contains(t, page, `href="/_admin/assets/styles.css"`)
	require.Contains(t, page, `rel="icon" href="data:,"`)
	require.NotRegexp(t, regexp.MustCompile(`(?i)\son[a-z]+\s*=`), page)
	for _, tag := range regexp.MustCompile(`(?is)<script[^>]*>`).FindAllString(page, -1) {
		require.Contains(t, tag, "src=")
	}
	require.NotContains(t, page, "http://")
	require.NotContains(t, page, "https://")

	script := string(javascript)
	for _, forbidden := range []string{
		"innerHTML",
		"outerHTML",
		"eval(",
		"localStorage",
		"sessionStorage",
		"indexedDB",
	} {
		require.NotContains(t, script, forbidden)
	}
	require.False(t, strings.Contains(script, "document.cookie"))
}
