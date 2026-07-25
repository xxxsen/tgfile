package s3

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/xxxsen/tgfile/server/handler/s3/s3base"
)

const maxResponseOverrideBytes = 8192

var responseOverrideHeaders = map[string]string{
	"response-cache-control":       "Cache-Control",
	"response-content-disposition": "Content-Disposition",
	"response-content-encoding":    "Content-Encoding",
	"response-content-language":    "Content-Language",
	"response-content-type":        "Content-Type",
	"response-expires":             "Expires",
}

type objectReadOptions struct {
	partNumber       *int
	responseOverride map[string]string
}

func hasResponseOverrideQuery(query url.Values) bool {
	for name := range responseOverrideHeaders {
		if hasQueryKey(query, name) {
			return true
		}
	}
	return false
}

func parseObjectReadOptions(request *http.Request) (*objectReadOptions, *s3base.APIError) {
	query := request.URL.Query()
	for _, name := range []string{
		"partNumber",
		"x-id",
		"response-cache-control",
		"response-content-disposition",
		"response-content-encoding",
		"response-content-language",
		"response-content-type",
		"response-expires",
	} {
		if len(query[name]) > 1 {
			return nil, invalidReadArgument("A read parameter was specified more than once.", nil)
		}
	}
	options := &objectReadOptions{responseOverride: make(map[string]string)}
	if values, exists := query["partNumber"]; exists {
		partNumber, err := parseBoundedDecimal(values[0], 1, maxMultipartPartNumber)
		if err != nil {
			return nil, invalidReadArgument("partNumber must be an integer from 1 through 10000.", err)
		}
		options.partNumber = &partNumber
		if request.Header.Get("Range") != "" {
			return nil, s3base.InvalidRequest("Range and partNumber cannot be combined.", nil)
		}
	}
	for queryName, headerName := range responseOverrideHeaders {
		values, exists := query[queryName]
		if !exists {
			continue
		}
		value := values[0]
		if value == "" || len(value) > maxResponseOverrideBytes || hasHTTPControl(value) {
			return nil, invalidReadArgument("A response header override is invalid.", nil)
		}
		if queryName == "response-expires" {
			parsed, err := http.ParseTime(value)
			if err != nil {
				return nil, invalidReadArgument("response-expires must be a valid HTTP date.", err)
			}
			value = parsed.UTC().Format(http.TimeFormat)
		}
		options.responseOverride[headerName] = value
	}
	return options, nil
}

func parseBoundedDecimal(value string, minimum, maximum int) (int, error) {
	if value == "" {
		return 0, errEmptyNonNegativeInteger
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return 0, errInvalidNonNegativeInteger
		}
	}
	parsed, err := strconv.ParseInt(value, 10, 32)
	if err != nil || parsed < int64(minimum) || parsed > int64(maximum) {
		return 0, errors.Join(errInvalidNonNegativeInteger, err)
	}
	return int(parsed), nil
}

func hasHTTPControl(value string) bool {
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return true
		}
	}
	return false
}

func invalidReadArgument(message string, cause error) *s3base.APIError {
	return s3base.NewError(http.StatusBadRequest, "InvalidArgument", message, cause)
}

func applyResponseOverrides(c *gin.Context, options *objectReadOptions) {
	if options == nil {
		return
	}
	for name, value := range options.responseOverride {
		c.Header(name, value)
	}
}

func hasUnsupportedObjectReadQuery(query url.Values) bool {
	for key := range query {
		if key == "partNumber" || key == "x-id" {
			continue
		}
		if _, supported := responseOverrideHeaders[key]; supported {
			continue
		}
		if strings.HasPrefix(key, "response-") {
			return true
		}
		switch key {
		case "accelerate", "acl", "analytics", "attributes", "cors", "delete",
			"encryption", "inventory", "legal-hold", "lifecycle", "location",
			"logging", "metrics", "notification", "object-lock", "ownershipControls",
			"policy", "publicAccessBlock", "replication", "requestPayment",
			"restore", "retention", "select", "select-type", "tagging", "torrent",
			"uploadId", "uploads", "versionId", "versioning", "website":
			return true
		}
	}
	return false
}
