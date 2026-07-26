package server_test

import (
	"bytes"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xxxsen/tgfile/server"
)

func TestWebDAVHTTPAndMutationMatrix(t *testing.T) {
	environment := newWebDAVIntegrationEnvironment(
		t,
		map[string]string{"editor": "secret"},
		server.WebDAVOptions{},
		20*1024*1024,
	)
	client := environment.server.Client()
	root := environment.server.URL + "/webdav/matrix"
	requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", "MKCOL", root, nil, nil,
	), http.StatusCreated)
	fileURL := root + "/file.txt"
	response := doWebDAVRequest(
		t, client, "editor", "secret", http.MethodPut, fileURL,
		strings.NewReader("0123456789"), nil,
	)
	requireWebDAVStatus(t, response, http.StatusCreated)
	etag := response.Header.Get("ETag")

	response = doWebDAVRequest(
		t, client, "editor", "secret", http.MethodGet, fileURL, nil,
		map[string]string{"Range": "bytes=2-5"},
	)
	require.Equal(t, []byte("2345"), requireWebDAVStatus(t, response, http.StatusPartialContent))
	require.Equal(t, "private, no-cache", response.Header.Get("Cache-Control"))
	require.NotContains(t, response.Header.Get("Cache-Control"), "public")

	response = doWebDAVRequest(
		t, client, "editor", "secret", http.MethodGet, fileURL, nil,
		map[string]string{"Range": "bytes=99-100"},
	)
	requireWebDAVStatus(t, response, http.StatusRequestedRangeNotSatisfiable)
	response = doWebDAVRequest(
		t, client, "editor", "secret", http.MethodGet, fileURL, nil,
		map[string]string{"If-Match": `"other"`},
	)
	require.Empty(t, requireWebDAVStatus(t, response, http.StatusPreconditionFailed))
	require.NotEqual(t, "10", response.Header.Get("Content-Length"))
	response = doWebDAVRequest(
		t, client, "editor", "secret", http.MethodGet, fileURL, nil,
		map[string]string{"If-None-Match": etag},
	)
	requireWebDAVStatus(t, response, http.StatusNotModified)
	response = doWebDAVRequest(
		t, client, "editor", "secret", http.MethodGet, fileURL, nil,
		map[string]string{"If-Match": "invalid"},
	)
	requireWebDAVStatus(t, response, http.StatusBadRequest)

	head := doWebDAVRequest(
		t, client, "editor", "secret", http.MethodHead, fileURL, nil, nil,
	)
	requireWebDAVStatus(t, head, http.StatusOK)
	require.Equal(t, etag, head.Header.Get("ETag"))
	response = doWebDAVRequest(
		t, client, "editor", "secret", http.MethodGet, fileURL, nil,
		map[string]string{"If-Modified-Since": head.Header.Get("Last-Modified")},
	)
	requireWebDAVStatus(t, response, http.StatusNotModified)
	requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", http.MethodHead, root+"/missing", nil, nil,
	), http.StatusNotFound)

	requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", http.MethodPut, root+"/missing/item",
		strings.NewReader("x"), nil,
	), http.StatusConflict)
	requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", http.MethodPut, root,
		strings.NewReader("x"), nil,
	), http.StatusMethodNotAllowed)
	requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", "MKCOL", root+"/body",
		strings.NewReader("<body/>"), map[string]string{"Content-Type": "application/xml"},
	), http.StatusUnsupportedMediaType)
	requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", http.MethodPut, fileURL,
		strings.NewReader("replacement"), map[string]string{"If-None-Match": "*"},
	), http.StatusPreconditionFailed)

	copyURL := root + "/copy.txt"
	requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", "COPY", fileURL, nil,
		map[string]string{"Destination": copyURL, "Overwrite": "F"},
	), http.StatusCreated)
	requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", "COPY", fileURL, nil,
		map[string]string{"Destination": copyURL, "Overwrite": "F"},
	), http.StatusPreconditionFailed)
	requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", "COPY", fileURL, nil,
		map[string]string{"Destination": copyURL, "Overwrite": "T"},
	), http.StatusNoContent)
	requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", "COPY", fileURL, nil,
		map[string]string{"Destination": copyURL, "Overwrite": "invalid"},
	), http.StatusBadRequest)
	requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", "COPY", fileURL, nil,
		map[string]string{"Destination": fileURL},
	), http.StatusForbidden)

	moveURL := root + "/moved.txt"
	requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", "MOVE", copyURL, nil,
		map[string]string{"Destination": moveURL, "Overwrite": "F"},
	), http.StatusCreated)
	requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", http.MethodHead, copyURL, nil, nil,
	), http.StatusNotFound)
	require.Equal(t, []byte("0123456789"), requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", http.MethodGet, moveURL, nil, nil,
	), http.StatusOK))

	requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", http.MethodDelete, root, nil,
		map[string]string{"Depth": "0"},
	), http.StatusBadRequest)
	requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", "PATCH", fileURL, nil, nil,
	), http.StatusMethodNotAllowed)
	requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", "PATCH", root+"/absent", nil, nil,
	), http.StatusNotFound)
}

func TestWebDAVPropertiesDepthAndCollectionCopy(t *testing.T) {
	environment := newWebDAVIntegrationEnvironment(
		t,
		map[string]string{"editor": "secret"},
		server.WebDAVOptions{},
		8*1024*1024,
	)
	client := environment.server.Client()
	root := environment.server.URL + "/webdav/properties"
	requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", "MKCOL", root, nil, nil,
	), http.StatusCreated)

	name := "space #中文😀.txt"
	fileURL := root + "/" + url.PathEscape(name)
	requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", http.MethodPut, fileURL,
		strings.NewReader("property-data"), nil,
	), http.StatusCreated)

	for _, depth := range []string{"", "infinity"} {
		response := doWebDAVRequest(
			t, client, "editor", "secret", "PROPFIND", root, nil,
			map[string]string{"Depth": depth},
		)
		require.Contains(
			t,
			string(requireWebDAVStatus(t, response, http.StatusForbidden)),
			"propfind-finite-depth",
		)
	}
	requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", "PROPFIND", root, nil,
		map[string]string{"Depth": "banana"},
	), http.StatusBadRequest)

	propnameBody := `<D:propfind xmlns:D="DAV:"><D:propname/></D:propfind>`
	propname := requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", "PROPFIND", fileURL,
		strings.NewReader(propnameBody), map[string]string{"Depth": "0"},
	), http.StatusMultiStatus)
	require.Contains(t, string(propname), "getetag")
	require.NotContains(t, string(propname), "property-data")

	propertyUpdate := `<D:propertyupdate xmlns:D="DAV:" xmlns:X="urn:tgfile:test">` +
		`<D:set><D:prop><X:color>blue</X:color><X:shape>round</X:shape></D:prop></D:set>` +
		`</D:propertyupdate>`
	requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", "PROPPATCH", root+"/missing.txt",
		strings.NewReader(`<D:propertyupdate xmlns:D="DAV:"><D:set><D:prop>`+
			`<D:getetag>bad</D:getetag></D:prop></D:set></D:propertyupdate>`),
		nil,
	), http.StatusNotFound)
	require.Contains(t, string(requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", "PROPPATCH", fileURL,
		strings.NewReader(propertyUpdate), nil,
	), http.StatusMultiStatus)), "200 OK")

	allProp := `<D:propfind xmlns:D="DAV:" xmlns:X="urn:tgfile:test">` +
		`<D:allprop/><D:include><X:missing/></D:include></D:propfind>`
	body := requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", "PROPFIND", fileURL,
		strings.NewReader(allProp), map[string]string{"Depth": "0"},
	), http.StatusMultiStatus)
	require.Contains(t, string(body), "blue")
	require.Contains(t, string(body), "round")
	require.Contains(t, string(body), "404 Not Found")
	require.Contains(
		t,
		string(body),
		">/webdav/properties/"+url.PathEscape(name)+"</href>",
	)

	atomicFailure := `<D:propertyupdate xmlns:D="DAV:" xmlns:X="urn:tgfile:test">` +
		`<D:set><D:prop><X:atomic>must-not-stick</X:atomic><D:getetag>bad</D:getetag>` +
		`</D:prop></D:set></D:propertyupdate>`
	failure := requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", "PROPPATCH", fileURL,
		strings.NewReader(atomicFailure), nil,
	), http.StatusMultiStatus)
	require.Contains(t, string(failure), "403 Forbidden")
	require.Contains(t, string(failure), "424 Failed Dependency")
	explicitAtomic := `<D:propfind xmlns:D="DAV:" xmlns:X="urn:tgfile:test">` +
		`<D:prop><X:atomic/></D:prop></D:propfind>`
	require.NotContains(t, string(requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", "PROPFIND", fileURL,
		strings.NewReader(explicitAtomic), map[string]string{"Depth": "0"},
	), http.StatusMultiStatus)), "must-not-stick")

	sourceCollection := root + "/source"
	requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", "MKCOL", sourceCollection, nil, nil,
	), http.StatusCreated)
	requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", http.MethodPut, sourceCollection+"/child.txt",
		strings.NewReader("child"), nil,
	), http.StatusCreated)
	destinationCollection := root + "/depth-zero"
	requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", "COPY", sourceCollection, nil,
		map[string]string{"Depth": "0", "Destination": destinationCollection},
	), http.StatusCreated)
	requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", http.MethodHead,
		destinationCollection+"/child.txt", nil, nil,
	), http.StatusNotFound)
}

func TestWebDAVExternalOriginMutationLimitAndSpoolLimit(t *testing.T) {
	tempDir := t.TempDir()
	environment := newWebDAVIntegrationEnvironment(
		t,
		map[string]string{"editor": "secret"},
		server.WebDAVOptions{
			ExternalOrigin:     "https://files.example.test",
			MaxUploadSize:      1024,
			UploadTempDir:      tempDir,
			MaxMutationEntries: 2,
		},
		8*1024*1024,
	)
	client := environment.server.Client()
	root := environment.server.URL + "/webdav/limits"
	requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", "MKCOL", root, nil, nil,
	), http.StatusCreated)

	source := root + "/source.txt"
	requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", http.MethodPut, source,
		strings.NewReader("source"), nil,
	), http.StatusCreated)
	requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", "COPY", source, nil,
		map[string]string{"Destination": "https://files.example.test/webdav/limits/copy.txt"},
	), http.StatusCreated)
	requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", "COPY", source, nil,
		map[string]string{
			"Destination": "https://files.example.test:443/webdav/limits/default-port.txt",
		},
	), http.StatusCreated)
	requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", "COPY", source, nil,
		map[string]string{"Destination": "http://files.example.test/webdav/limits/wrong-scheme.txt"},
	), http.StatusBadGateway)
	requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", "COPY", source, nil,
		map[string]string{
			"Destination": "https://files.example.test/webdav/limits/query.txt?ignored=true",
		},
	), http.StatusBadRequest)
	requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", "COPY", source, nil,
		map[string]string{
			"Destination": "//other.invalid/webdav/limits/network-path.txt",
		},
	), http.StatusBadRequest)
	lockBody := `<D:lockinfo xmlns:D="DAV:"><D:lockscope><D:exclusive/>` +
		`</D:lockscope><D:locktype><D:write/></D:locktype></D:lockinfo>`
	lockResponse := doWebDAVRequest(
		t, client, "editor", "secret", "LOCK", source,
		strings.NewReader(lockBody), map[string]string{"Depth": "0"},
	)
	requireWebDAVStatus(t, lockResponse, http.StatusOK)
	lockTokenHeader := lockResponse.Header.Get("Lock-Token")
	lockToken := strings.Trim(lockTokenHeader, "<>")
	taggedSource := "https://files.example.test/webdav/limits/source.txt"
	requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", http.MethodPut, source,
		strings.NewReader("updated"),
		map[string]string{"If": "<" + taggedSource + "> (<" + lockToken + ">)"},
	), http.StatusNoContent)
	requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", "UNLOCK", source, nil,
		map[string]string{"Lock-Token": lockTokenHeader},
	), http.StatusNoContent)

	tree := root + "/tree"
	requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", "MKCOL", tree, nil, nil,
	), http.StatusCreated)
	for index := 0; index < 2; index++ {
		requireWebDAVStatus(t, doWebDAVRequest(
			t, client, "editor", "secret", http.MethodPut,
			fmt.Sprintf("%s/%d.txt", tree, index),
			strings.NewReader("x"), nil,
		), http.StatusCreated)
	}
	requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", "COPY", tree, nil,
		map[string]string{
			"Depth":       "0",
			"Destination": "https://files.example.test/webdav/limits/depth-zero",
		},
	), http.StatusCreated)
	requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", http.MethodDelete, tree, nil, nil,
	), http.StatusInsufficientStorage)

	oversized := &unknownLengthReader{reader: bytes.NewReader(bytes.Repeat([]byte("x"), 1025))}
	requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", http.MethodPut, root+"/too-large.bin",
		oversized, nil,
	), http.StatusRequestEntityTooLarge)
	entries, err := tempDirEntries(tempDir)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestWebDAVLockScopeAndIncrementalSync(t *testing.T) {
	environment := newWebDAVIntegrationEnvironment(
		t,
		map[string]string{"editor": "secret"},
		server.WebDAVOptions{SyncPageSize: 1},
		8*1024*1024,
	)
	client := environment.server.Client()
	root := environment.server.URL + "/webdav/scope"
	requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", "MKCOL", root, nil, nil,
	), http.StatusCreated)
	child := root + "/child.txt"
	requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", http.MethodPut, child,
		strings.NewReader("old"), nil,
	), http.StatusCreated)
	nested := root + "/nested"
	requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", "MKCOL", nested, nil, nil,
	), http.StatusCreated)
	requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", http.MethodPut, nested+"/deep.txt",
		strings.NewReader("deep"), nil,
	), http.StatusCreated)
	initialLevelOne := requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", "REPORT", root,
		strings.NewReader(syncReportBody("")), map[string]string{"Depth": "0"},
	), http.StatusMultiStatus)
	require.Contains(t, string(initialLevelOne), "/nested/")
	require.NotContains(t, string(initialLevelOne), "deep.txt")
	initialInfinite := requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", "REPORT", root,
		strings.NewReader(syncReportBodyAtLevel("", "infinite")),
		map[string]string{"Depth": "0"},
	), http.StatusMultiStatus)
	require.Contains(t, string(initialInfinite), "deep.txt")

	lockBody := `<D:lockinfo xmlns:D="DAV:"><D:lockscope><D:exclusive/>` +
		`</D:lockscope><D:locktype><D:write/></D:locktype></D:lockinfo>`
	lockResponse := doWebDAVRequest(
		t, client, "editor", "secret", "LOCK", root,
		strings.NewReader(lockBody), map[string]string{"Depth": "infinity"},
	)
	requireWebDAVStatus(t, lockResponse, http.StatusOK)
	tokenHeader := lockResponse.Header.Get("Lock-Token")
	token := strings.Trim(tokenHeader, "<>")
	requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", http.MethodPut, child,
		strings.NewReader("blocked"), nil,
	), http.StatusLocked)
	requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", "LOCK", child,
		strings.NewReader(lockBody), map[string]string{"Depth": "0"},
	), http.StatusLocked)
	requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", http.MethodPut, child,
		strings.NewReader("updated"), map[string]string{"If": "(<" + token + ">)"},
	), http.StatusNoContent)
	requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", "UNLOCK", root, nil,
		map[string]string{"Lock-Token": tokenHeader},
	), http.StatusNoContent)

	initialToken := readSyncToken(t, requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", "REPORT", root,
		strings.NewReader(syncReportBody("")), map[string]string{"Depth": "0"},
	), http.StatusMultiStatus))
	added := root + "/added.txt"
	requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", http.MethodPut, added,
		strings.NewReader("added"), nil,
	), http.StatusCreated)
	firstPage := requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", "REPORT", root,
		strings.NewReader(syncReportBody(initialToken)), map[string]string{"Depth": "0"},
	), http.StatusMultiStatus)
	require.Contains(t, string(firstPage), "added.txt")
	nextToken := readSyncToken(t, firstPage)

	requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", http.MethodDelete, added, nil, nil,
	), http.StatusNoContent)
	tombstone := requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", "REPORT", root,
		strings.NewReader(syncReportBody(nextToken)), map[string]string{"Depth": "0"},
	), http.StatusMultiStatus)
	require.Contains(t, string(tombstone), "404 Not Found")
	require.Contains(t, string(tombstone), "added.txt")

	unknownSyncRevision := int64(999999999999)
	invalid := requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", "REPORT", root,
		strings.NewReader(syncReportBody(fmt.Sprintf(
			"urn:tgfile:webdav-sync:%d",
			unknownSyncRevision,
		))), map[string]string{"Depth": "0"},
	), http.StatusForbidden)
	require.Contains(t, string(invalid), "valid-sync-token")
}

func TestWebDAVTelegramPartBoundary(t *testing.T) {
	const blockSize = 20 * 1024 * 1024
	environment := newWebDAVIntegrationEnvironment(
		t,
		map[string]string{"editor": "secret"},
		server.WebDAVOptions{MaxUploadSize: blockSize + 1},
		blockSize,
	)
	client := environment.server.Client()
	root := environment.server.URL + "/webdav/parts"
	requireWebDAVStatus(t, doWebDAVRequest(
		t, client, "editor", "secret", "MKCOL", root, nil, nil,
	), http.StatusCreated)

	for _, size := range []int{blockSize, blockSize + 1} {
		fillByte := byte(20)
		if size > blockSize {
			fillByte = 21
		}
		content := bytes.Repeat([]byte{fillByte}, size)
		target := fmt.Sprintf("%s/%d.bin", root, size)
		requireWebDAVStatus(t, doWebDAVRequest(
			t, client, "editor", "secret", http.MethodPut, target,
			bytes.NewReader(content), nil,
		), http.StatusCreated)
		rangeStart := blockSize - 2
		response := doWebDAVRequest(
			t, client, "editor", "secret", http.MethodGet, target, nil,
			map[string]string{
				"Range": fmt.Sprintf("bytes=%d-%d", rangeStart, size-1),
			},
		)
		require.Equal(
			t,
			content[rangeStart:],
			requireWebDAVStatus(t, response, http.StatusPartialContent),
		)
		require.Equal(t, fmt.Sprintf("%d", size), doWebDAVRequest(
			t, client, "editor", "secret", http.MethodHead, target, nil, nil,
		).Header.Get("Content-Length"))
	}
}

func syncReportBody(token string) string {
	return syncReportBodyAtLevel(token, "1")
}

func syncReportBodyAtLevel(token, level string) string {
	return `<D:sync-collection xmlns:D="DAV:"><D:sync-token>` + token +
		`</D:sync-token><D:sync-level>` + level + `</D:sync-level>` +
		`<D:prop><D:getetag/></D:prop></D:sync-collection>`
}

func readSyncToken(t *testing.T, body []byte) string {
	t.Helper()
	matches := regexp.MustCompile(`urn:tgfile:webdav-sync:[0-9]+`).Find(body)
	require.NotEmpty(t, matches)
	return string(matches)
}

func tempDirEntries(path string) ([]string, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names, nil
}
