package webdav

import "net/http"

var AllowMethods = []string{
	http.MethodGet,
	http.MethodPut,
	http.MethodDelete,
	http.MethodHead,
	http.MethodOptions,
	"PROPPATCH",
	"PROPFIND",
	"COPY",
	"MOVE",
	"MKCOL",
}
