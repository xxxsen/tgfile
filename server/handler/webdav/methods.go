package webdav

import "net/http"

var AllowMethods = []string{
	http.MethodOptions,
	http.MethodGet,
	http.MethodHead,
	http.MethodPut,
	http.MethodDelete,
	"PROPFIND",
	"PROPPATCH",
	"MKCOL",
	"COPY",
	"MOVE",
	"LOCK",
	"UNLOCK",
	"REPORT",
}

var ReadOnlyMethods = []string{
	http.MethodOptions,
	http.MethodGet,
	http.MethodHead,
	"PROPFIND",
	"REPORT",
}
