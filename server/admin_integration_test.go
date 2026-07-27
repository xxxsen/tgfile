package server_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xxxsen/tgfile/backupfmt"
	"github.com/xxxsen/tgfile/backupmgr"
	"github.com/xxxsen/tgfile/blockio/mem"
	"github.com/xxxsen/tgfile/db"
	"github.com/xxxsen/tgfile/filemgr"
	"github.com/xxxsen/tgfile/server"
)

const (
	adminTestOrigin            = "http://127.0.0.1"
	adminTestAlternativeOrigin = "http://localhost"
)

type adminSessionResponse struct {
	Username  string `json:"username"`
	Role      string `json:"role"`
	CSRFToken string `json:"csrf_token"`
}

type adminJobResponse struct {
	JobID             string `json:"job_id"`
	Owner             string `json:"owner"`
	State             string `json:"state"`
	ArtifactSHA256    string `json:"artifact_sha256"`
	ArtifactAvailable bool   `json:"artifact_available"`
}

type adminEntryResponse struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
	Size int64  `json:"size"`
	ETag string `json:"etag"`
}

func TestAdminDisabledAndAdminSecurityHeaders(t *testing.T) {
	disabled, err := server.New("127.0.0.1:0")
	require.NoError(t, err)
	response := serveAdminRequest(t, disabled, http.MethodGet, "/_admin/", nil, nil)
	require.Equal(t, http.StatusNotFound, response.Code)

	environment := newAdminTestEnvironment(t)
	response = serveAdminRequest(t, environment.handler, http.MethodGet, "/_admin/", nil, nil)
	require.Equal(t, http.StatusOK, response.Code)
	require.Contains(t, response.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'")
	require.Equal(t, "no-store", response.Header().Get("Cache-Control"))
	require.Equal(t, "nosniff", response.Header().Get("X-Content-Type-Options"))
	require.Empty(t, response.Header().Get("Access-Control-Allow-Origin"))
	require.Contains(t, response.Body.String(), `src="/_admin/assets/app.js"`)

	response = serveAdminRequest(t, environment.handler, http.MethodHead, "/_admin/", nil, nil)
	require.Equal(t, http.StatusOK, response.Code)
	require.Empty(t, response.Body.String())

	response = serveAdminRequest(
		t,
		environment.handler,
		http.MethodGet,
		"/_admin/assets/app.js",
		nil,
		nil,
	)
	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, "no-cache", response.Header().Get("Cache-Control"))
	etag := response.Header().Get("ETag")
	require.NotEmpty(t, etag)

	response = serveAdminRequest(
		t,
		environment.handler,
		http.MethodHead,
		"/_admin/assets/app.js",
		nil,
		nil,
	)
	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, etag, response.Header().Get("ETag"))
	require.Empty(t, response.Body.String())

	response = serveAdminRequest(
		t,
		environment.handler,
		http.MethodGet,
		"/_admin/assets/app.js",
		nil,
		map[string]string{"If-None-Match": etag},
	)
	require.Equal(t, http.StatusNotModified, response.Code)
	require.Empty(t, response.Body.String())

	response = serveAdminRequest(
		t,
		environment.handler,
		http.MethodGet,
		"/_admin/api/v1/not-a-route",
		nil,
		nil,
	)
	require.Equal(t, http.StatusNotFound, response.Code)
	require.Equal(t, "no-store", response.Header().Get("Cache-Control"))
	require.Equal(t, "nosniff", response.Header().Get("X-Content-Type-Options"))

	response = serveAdminRequest(
		t,
		environment.handler,
		http.MethodGet,
		"/_admin/api/v1/session",
		nil,
		nil,
	)
	require.Equal(t, http.StatusUnauthorized, response.Code)
	require.Equal(t, "nosniff", response.Header().Get("X-Content-Type-Options"))

	response = serveAdminRequest(
		t,
		environment.handler,
		http.MethodPost,
		"/_admin/api/v1/session",
		bytes.NewBufferString(`{"username":"operator","password":"write-secret"}`),
		map[string]string{
			"Content-Type": "application/json",
			"Origin":       "http://attacker.invalid",
		},
	)
	require.Equal(t, http.StatusForbidden, response.Code)
	require.NotContains(t, response.Body.String(), "operator")
	require.NotContains(t, response.Body.String(), "write-secret")

	response = serveAdminRequest(
		t,
		environment.handler,
		http.MethodPost,
		"/_admin/api/v1/session",
		bytes.NewBufferString(`{"username":"operator","password":"write-secret"}`),
		map[string]string{
			"Content-Type": "application/json",
			"Origin":       adminTestAlternativeOrigin,
		},
	)
	require.Equal(t, http.StatusOK, response.Code)
	require.NotEmpty(t, response.Header().Values("Set-Cookie"))

	response = serveAdminRequest(
		t,
		environment.handler,
		http.MethodPost,
		"/_admin/api/v1/session?unexpected=true",
		bytes.NewBufferString(`{"username":"operator","password":"write-secret"}`),
		map[string]string{
			"Content-Type": "application/json",
			"Origin":       adminTestOrigin,
		},
	)
	require.Equal(t, http.StatusBadRequest, response.Code)

	messages := make([]string, 0, 2)
	for _, credentials := range [][2]string{
		{"operator", "wrong"},
		{"unknown-user", "wrong"},
	} {
		started := time.Now()
		response = serveAdminRequest(
			t,
			environment.handler,
			http.MethodPost,
			"/_admin/api/v1/session",
			bytes.NewBufferString(
				`{"username":`+quotedJSON(credentials[0])+
					`,"password":`+quotedJSON(credentials[1])+`}`,
			),
			map[string]string{
				"Content-Type": "application/json",
				"Origin":       adminTestOrigin,
			},
		)
		require.Equal(t, http.StatusUnauthorized, response.Code)
		require.GreaterOrEqual(t, time.Since(started), 240*time.Millisecond)
		var envelope struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &envelope))
		require.Equal(t, "invalid_credentials", envelope.Error.Code)
		messages = append(messages, envelope.Error.Message)
	}
	require.Equal(t, messages[0], messages[1])
}

func TestAdminFileAndBackupWorkflowWithRoleIsolation(t *testing.T) {
	environment := newAdminTestEnvironment(t)
	testServer := httptest.NewServer(environment.handler)
	defer testServer.Close()

	viewerClient := adminHTTPClient(t)
	viewer := loginAdmin(t, viewerClient, testServer.URL, "viewer", "view-secret")
	require.Equal(t, "read", viewer.Role)
	operatorClient := adminHTTPClient(t)
	operator := loginAdmin(t, operatorClient, testServer.URL, "operator", "write-secret")
	require.Equal(t, "read-write", operator.Role)

	response := doAdminRequest(
		t,
		operatorClient,
		http.MethodGet,
		testServer.URL+"/_admin/api/v1/session?unexpected=true",
		nil,
		operator,
		nil,
	)
	require.Equal(t, http.StatusBadRequest, response.StatusCode)
	closeResponse(t, response)

	request, err := http.NewRequestWithContext(
		t.Context(),
		http.MethodPut,
		testServer.URL+"/_admin/api/v1/content?path=%2Fuploads%2Fchunked.bin",
		io.NopCloser(bytes.NewBufferString("chunked")),
	)
	require.NoError(t, err)
	request.ContentLength = -1
	request.TransferEncoding = []string{"chunked"}
	request.Header.Set("Origin", adminTestOrigin)
	request.Header.Set("X-Csrf-Token", operator.CSRFToken)
	request.Header.Set("If-None-Match", "*")
	response, err = operatorClient.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusLengthRequired, response.StatusCode)
	closeResponse(t, response)

	response = doAdminRequest(
		t,
		viewerClient,
		http.MethodPut,
		testServer.URL+"/_admin/api/v1/content?path=%2Fuploads%2Fdenied.txt",
		bytes.NewBufferString("x"),
		viewer,
		map[string]string{"If-None-Match": "*"},
	)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	closeResponse(t, response)

	canceled := createAdminExport(t, viewerClient, testServer.URL, viewer, "/", "viewer-cancel")
	response = doAdminRequest(
		t,
		viewerClient,
		http.MethodPost,
		testServer.URL+"/_admin/api/v1/backup/jobs/"+canceled.JobID+"/cancel",
		nil,
		viewer,
		nil,
	)
	defer response.Body.Close()
	require.Equal(t, http.StatusAccepted, response.StatusCode)
	closeResponse(t, response)

	runContext, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go func() { _ = environment.manager.Run(runContext) }()

	uploadAdminFile(t, operatorClient, testServer.URL, operator, "/uploads/empty.bin", nil, "*", http.StatusCreated)
	uploadAdminFile(
		t,
		operatorClient,
		testServer.URL,
		operator,
		"/uploads/too-large.bin",
		bytes.Repeat([]byte("x"), 1025),
		"*",
		http.StatusRequestEntityTooLarge,
	)
	large := []byte("0123456789")
	uploadAdminFile(
		t,
		operatorClient,
		testServer.URL,
		operator,
		"/uploads/空 file.bin",
		large,
		"*",
		http.StatusCreated,
	)
	entry := statAdminEntry(
		t,
		operatorClient,
		testServer.URL,
		"/uploads/空 file.bin",
	)
	require.Equal(t, int64(len(large)), entry.Size)
	require.Equal(t, "file", entry.Kind)
	require.NotEmpty(t, entry.ETag)

	response = doAdminRequest(
		t,
		operatorClient,
		http.MethodHead,
		testServer.URL+"/_admin/api/v1/content?path=%2Fuploads%2F%E7%A9%BA+file.bin",
		nil,
		operator,
		nil,
	)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, entry.ETag, response.Header.Get("ETag"))
	require.Equal(t, strconv.Itoa(len(large)), response.Header.Get("Content-Length"))
	require.NoError(t, response.Body.Close())

	response = doAdminRequest(
		t,
		operatorClient,
		http.MethodGet,
		testServer.URL+"/_admin/api/v1/content?path=%2Fuploads%2F%E7%A9%BA+file.bin",
		nil,
		operator,
		map[string]string{"If-None-Match": entry.ETag},
	)
	require.Equal(t, http.StatusNotModified, response.StatusCode)
	closeResponse(t, response)

	response = doAdminRequest(
		t,
		operatorClient,
		http.MethodGet,
		testServer.URL+"/_admin/api/v1/content?path=%2Fuploads%2F%E7%A9%BA+file.bin",
		nil,
		operator,
		map[string]string{"Range": "bytes=2-6"},
	)
	require.Equal(t, http.StatusPartialContent, response.StatusCode)
	raw, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, large[2:7], raw)
	require.Equal(t, "nosniff", response.Header.Get("X-Content-Type-Options"))
	require.Contains(t, response.Header.Get("Content-Disposition"), "filename*=UTF-8''")

	uploadAdminFile(
		t,
		operatorClient,
		testServer.URL,
		operator,
		"/uploads/空 file.bin",
		[]byte("stale"),
		`"1-1"`,
		http.StatusPreconditionFailed,
	)
	replacement := []byte("new-content")
	uploadAdminFile(
		t,
		operatorClient,
		testServer.URL,
		operator,
		"/uploads/空 file.bin",
		replacement,
		entry.ETag,
		http.StatusOK,
	)
	require.Equal(
		t,
		replacement,
		downloadAdminFile(t, operatorClient, testServer.URL, operator, "/uploads/空 file.bin"),
	)

	listed := listAllAdminEntries(t, operatorClient, testServer.URL, operator, "/uploads", 1)
	require.Equal(t, []string{"/uploads/empty.bin", "/uploads/空 file.bin"}, listed)

	exportJob := createAdminExport(
		t,
		operatorClient,
		testServer.URL,
		operator,
		"/",
		"operator-export",
	)
	exportJob = waitAdminJob(t, operatorClient, testServer.URL, operator, exportJob.JobID)
	require.Equal(t, "succeeded", exportJob.State)
	require.True(t, exportJob.ArtifactAvailable)

	response = doAdminRequest(
		t,
		viewerClient,
		http.MethodGet,
		testServer.URL+"/_admin/api/v1/backup/jobs/"+exportJob.JobID,
		nil,
		viewer,
		nil,
	)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	closeResponse(t, response)

	response = doAdminRequest(
		t,
		operatorClient,
		http.MethodGet,
		testServer.URL+"/_admin/api/v1/backup/exports/"+exportJob.JobID+"/artifact",
		nil,
		operator,
		nil,
	)
	require.Equal(t, http.StatusOK, response.StatusCode)
	artifact, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	sum := sha256.Sum256(artifact)
	require.Equal(t, exportJob.ArtifactSHA256, hex.EncodeToString(sum[:]))

	importURL := testServer.URL +
		"/_admin/api/v1/backup/imports?conflict=replace&dry_run=true"
	response = doAdminRequest(
		t,
		operatorClient,
		http.MethodPost,
		importURL,
		bytes.NewReader(artifact),
		operator,
		map[string]string{
			"Content-Type":    backupfmt.MediaType,
			"Idempotency-Key": "operator-import-dry",
		},
	)
	defer response.Body.Close()
	require.Equal(t, http.StatusAccepted, response.StatusCode)
	importJob := decodeAdminData[adminJobResponse](t, response)
	require.Equal(t, exportJob.ArtifactSHA256, importJob.ArtifactSHA256)
	importJob = waitAdminJob(t, operatorClient, testServer.URL, operator, importJob.JobID)
	require.Equal(t, "succeeded", importJob.State)

	response = doAdminRequest(
		t,
		operatorClient,
		http.MethodGet,
		testServer.URL+"/backup/v2/jobs/"+exportJob.JobID,
		nil,
		operator,
		nil,
	)
	require.Equal(t, http.StatusNotFound, response.StatusCode)
	closeResponse(t, response)
}

type adminTestEnvironment struct {
	handler http.Handler
	manager *backupmgr.Manager
}

func newAdminTestEnvironment(t *testing.T) adminTestEnvironment {
	t.Helper()
	databaseClient, err := db.Open(filepath.Join(t.TempDir(), "data.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, databaseClient.Close()) })
	block, err := mem.New(4)
	require.NoError(t, err)
	cache, err := filemgr.NewFileIOCache(&filemgr.FileIOCacheConfig{
		DisableL1Cache: true,
		DisableL2Cache: true,
	})
	require.NoError(t, err)
	registerIntegrationCacheCleanup(t, cache)
	files := filemgr.NewFileManager(databaseClient, block, cache)
	require.NoError(t, files.CreateFileLink(t.Context(), "/uploads", 0, 0, true))
	manager, err := backupmgr.New(databaseClient, files, backupmgr.Options{
		WorkDir: filepath.Join(t.TempDir(), "backup-work"),
		Limits:  backupfmt.DefaultLimits(), SchemaVersion: 13,
		MaxPartSize:       files.BackupMaxPartSize(),
		ArtifactRetention: time.Hour,
		JobRetention:      24 * time.Hour,
	})
	require.NoError(t, err)
	users := map[string]string{"viewer": "view-secret", "operator": "write-secret"}
	handler, err := server.New(
		"127.0.0.1:0",
		server.WithUser(users),
		server.WithFileManager(files),
		server.WithBackup(server.BackupOptions{Enabled: false}, manager),
		server.WithAdmin(server.AdminOptions{
			Enabled: true,
			ExternalOrigins: []string{
				adminTestOrigin,
				adminTestAlternativeOrigin,
			},
			Users:       map[string]string{"viewer": "read", "operator": "read-write"},
			SessionIdle: 30 * time.Minute, SessionMaximum: 12 * time.Hour,
			MaxUploadSize: 1024, MaxPathBytes: 1024, MaxMutationEntries: 1000,
		}),
	)
	require.NoError(t, err)
	return adminTestEnvironment{handler: handler, manager: manager}
}

func serveAdminRequest(
	t *testing.T,
	handler http.Handler,
	method, target string,
	body io.Reader,
	headers map[string]string,
) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequestWithContext(t.Context(), method, target, body)
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func adminHTTPClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	return &http.Client{Jar: jar, Timeout: 10 * time.Second}
}

func loginAdmin(
	t *testing.T,
	client *http.Client,
	baseURL, username, password string,
) adminSessionResponse {
	t.Helper()
	request, err := http.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		baseURL+"/_admin/api/v1/session",
		bytes.NewBufferString(`{"username":`+quotedJSON(username)+`,"password":`+quotedJSON(password)+`}`),
	)
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", adminTestOrigin)
	response, err := client.Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	session := decodeAdminData[adminSessionResponse](t, response)
	require.NotEmpty(t, session.CSRFToken)
	require.Len(t, response.Cookies(), 1)
	cookie := response.Cookies()[0]
	require.Equal(t, "/_admin/", cookie.Path)
	require.True(t, cookie.HttpOnly)
	require.Equal(t, http.SameSiteStrictMode, cookie.SameSite)
	require.False(t, cookie.Secure)
	return session
}

func quotedJSON(value string) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}

func urlQueryEscape(value string) string {
	return url.QueryEscape(value)
}

func quotedInteger(value int) string {
	return strconv.Itoa(value)
}

func doAdminRequest(
	t *testing.T,
	client *http.Client,
	method, target string,
	body io.Reader,
	session adminSessionResponse,
	headers map[string]string,
) *http.Response {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), method, target, body)
	require.NoError(t, err)
	if method == http.MethodPost || method == http.MethodPut || method == http.MethodDelete {
		request.Header.Set("Origin", adminTestOrigin)
		request.Header.Set("X-Csrf-Token", session.CSRFToken)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := client.Do(request)
	require.NoError(t, err)
	return response
}

func uploadAdminFile(
	t *testing.T,
	client *http.Client,
	baseURL string,
	session adminSessionResponse,
	path string,
	content []byte,
	condition string,
	expectedStatus int,
) {
	t.Helper()
	headerName := "If-Match"
	if condition == "*" {
		headerName = "If-None-Match"
	}
	response := doAdminRequest(
		t,
		client,
		http.MethodPut,
		baseURL+"/_admin/api/v1/content?path="+urlQueryEscape(path),
		bytes.NewReader(content),
		session,
		map[string]string{headerName: condition},
	)
	defer response.Body.Close()
	require.Equal(t, expectedStatus, response.StatusCode)
	closeResponse(t, response)
}

func statAdminEntry(
	t *testing.T,
	client *http.Client,
	baseURL, path string,
) adminEntryResponse {
	t.Helper()
	response := doAdminRequest(
		t,
		client,
		http.MethodGet,
		baseURL+"/_admin/api/v1/entries/stat?path="+urlQueryEscape(path),
		nil,
		adminSessionResponse{},
		nil,
	)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	return decodeAdminData[adminEntryResponse](t, response)
}

func downloadAdminFile(
	t *testing.T,
	client *http.Client,
	baseURL string,
	session adminSessionResponse,
	path string,
) []byte {
	t.Helper()
	response := doAdminRequest(
		t,
		client,
		http.MethodGet,
		baseURL+"/_admin/api/v1/content?path="+urlQueryEscape(path),
		nil,
		session,
		nil,
	)
	require.Equal(t, http.StatusOK, response.StatusCode)
	raw, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	return raw
}

func listAllAdminEntries(
	t *testing.T,
	client *http.Client,
	baseURL string,
	session adminSessionResponse,
	path string,
	limit int,
) []string {
	t.Helper()
	var (
		cursor string
		items  []string
	)
	for {
		target := baseURL + "/_admin/api/v1/entries?path=" + urlQueryEscape(path) +
			"&limit=" + quotedInteger(limit)
		if cursor != "" {
			target += "&cursor=" + urlQueryEscape(cursor)
		}
		response := doAdminRequest(t, client, http.MethodGet, target, nil, session, nil)
		defer response.Body.Close()
		require.Equal(t, http.StatusOK, response.StatusCode)
		page := decodeAdminData[struct {
			Items      []adminEntryResponse `json:"items"`
			NextCursor string               `json:"next_cursor"`
		}](t, response)
		for _, item := range page.Items {
			items = append(items, item.Path)
		}
		cursor = page.NextCursor
		if cursor == "" {
			return items
		}
	}
}

func createAdminExport(
	t *testing.T,
	client *http.Client,
	baseURL string,
	session adminSessionResponse,
	scope, key string,
) adminJobResponse {
	t.Helper()
	response := doAdminRequest(
		t,
		client,
		http.MethodPost,
		baseURL+"/_admin/api/v1/backup/exports",
		bytes.NewBufferString(`{"scope":`+quotedJSON(scope)+`}`),
		session,
		map[string]string{"Content-Type": "application/json", "Idempotency-Key": key},
	)
	defer response.Body.Close()
	require.Equal(t, http.StatusAccepted, response.StatusCode)
	return decodeAdminData[adminJobResponse](t, response)
}

func waitAdminJob(
	t *testing.T,
	client *http.Client,
	baseURL string,
	session adminSessionResponse,
	jobID string,
) adminJobResponse {
	t.Helper()
	var job adminJobResponse
	require.Eventually(t, func() bool {
		response := doAdminRequest(
			t,
			client,
			http.MethodGet,
			baseURL+"/_admin/api/v1/backup/jobs/"+jobID,
			nil,
			session,
			nil,
		)
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return false
		}
		job = decodeAdminDataWithoutClose[adminJobResponse](t, response.Body)
		return job.State == "succeeded" || job.State == "failed" || job.State == "canceled"
	}, 5*time.Second, 25*time.Millisecond)
	return job
}

func decodeAdminData[T any](t *testing.T, response *http.Response) T {
	t.Helper()
	defer response.Body.Close()
	return decodeAdminDataWithoutClose[T](t, response.Body)
}

func decodeAdminDataWithoutClose[T any](t *testing.T, body io.Reader) T {
	t.Helper()
	var envelope struct {
		Data T `json:"data"`
	}
	require.NoError(t, json.NewDecoder(body).Decode(&envelope))
	return envelope.Data
}

func closeResponse(t *testing.T, response *http.Response) {
	t.Helper()
	_, err := io.Copy(io.Discard, response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
}
