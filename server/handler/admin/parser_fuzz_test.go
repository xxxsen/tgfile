package admin

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func FuzzDecodeStrictLoginJSON(f *testing.F) {
	f.Add(`{"username":"operator","password":"secret"}`)
	f.Add(`{"username":"x","username":"y","password":"z"}`)
	f.Add(`not-json`)
	f.Fuzz(func(_ *testing.T, input string) {
		var request loginRequest
		_ = decodeStrictJSON(strings.NewReader(input), 16*1024, &request)
	})
}

func FuzzDecodeEntryCursor(f *testing.F) {
	f.Add("")
	f.Add("eyJ2IjoxfQ")
	f.Add("not-base64")
	f.Fuzz(func(_ *testing.T, input string) {
		_, _, _ = decodeEntryCursor(input)
	})
}

func FuzzDecodeJobCursor(f *testing.F) {
	f.Add("")
	f.Add("eyJ2IjoxfQ")
	f.Add("not-base64")
	f.Fuzz(func(_ *testing.T, input string) {
		_, _, _ = decodeJobCursor(input)
	})
}

func FuzzAdminPathAndQueryParsing(f *testing.F) {
	f.Add("/hackmd/空 file.txt", "path=%2Fhackmd%2F%E7%A9%BA+file.txt")
	f.Add("../escape", "path=%252fescape")
	f.Add("/control\x00", "%zz")
	f.Fuzz(func(_ *testing.T, resourcePath, rawQuery string) {
		recorder := httptest.NewRecorder()
		context, _ := gin.CreateTestContext(recorder)
		context.Request = &http.Request{URL: &url.URL{RawQuery: rawQuery}}
		handler := &Handler{maxPathBytes: 1024}
		_, _ = handler.parsePath(context, resourcePath)
		_, _ = handler.parseQuery(context, "path", "limit", "cursor")
	})
}
