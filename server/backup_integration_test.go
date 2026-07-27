package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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

func TestBackupV2HTTPExportAuthorizationAndArtifact(t *testing.T) {
	databaseClient, err := db.Open(filepath.Join(t.TempDir(), "data.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, databaseClient.Close()) })
	block, err := mem.New(4)
	require.NoError(t, err)
	cache, err := filemgr.NewFileIOCache(&filemgr.FileIOCacheConfig{
		DisableL1Cache: true, DisableL2Cache: true,
	})
	require.NoError(t, err)
	registerIntegrationCacheCleanup(t, cache)
	files := filemgr.NewFileManager(databaseClient, block, cache)
	fileID, err := files.CreateFile(t.Context(), 6, bytes.NewBufferString("backup"))
	require.NoError(t, err)
	require.NoError(t, files.CreateFileLink(t.Context(), "/data.txt", fileID, 6, false))

	manager, err := backupmgr.New(databaseClient, files, backupmgr.Options{
		WorkDir: filepath.Join(t.TempDir(), "backup-work"),
		Limits:  backupfmt.DefaultLimits(), SchemaVersion: 13,
		MaxPartSize:       files.BackupMaxPartSize(),
		ArtifactRetention: time.Hour, JobRetention: 24 * time.Hour,
	})
	require.NoError(t, err)
	users := map[string]string{"operator": "secret", "reader": "secret"}
	handler, err := server.New(
		"127.0.0.1:0",
		server.WithUser(users),
		server.WithFileManager(files),
		server.WithBackup(server.BackupOptions{
			Enabled: true,
			Users:   map[string]string{"operator": "read-write", "reader": "read"},
		}, manager),
	)
	require.NoError(t, err)
	testServer := httptest.NewServer(handler)
	defer testServer.Close()
	runContext, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = manager.Run(runContext) }()

	request, err := http.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		testServer.URL+"/backup/v2/exports",
		bytes.NewBufferString(`{"scope":"/"}`),
	)
	require.NoError(t, err)
	request.SetBasicAuth("operator", "secret")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "http-export")
	response, err := testServer.Client().Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusAccepted, response.StatusCode)
	var created backupmgr.Job
	require.NoError(t, json.NewDecoder(response.Body).Decode(&created))
	require.NoError(t, response.Body.Close())

	var completed backupmgr.Job
	require.Eventually(t, func() bool {
		jobRequest, err := http.NewRequestWithContext(
			t.Context(),
			http.MethodGet,
			testServer.URL+"/backup/v2/jobs/"+created.JobID,
			nil,
		)
		if err != nil {
			return false
		}
		jobRequest.SetBasicAuth("operator", "secret")
		jobResponse, err := testServer.Client().Do(jobRequest)
		if err != nil {
			return false
		}
		defer jobResponse.Body.Close()
		if jobResponse.StatusCode != http.StatusOK {
			return false
		}
		if json.NewDecoder(jobResponse.Body).Decode(&completed) != nil {
			return false
		}
		return completed.State == "succeeded"
	}, 5*time.Second, 25*time.Millisecond)
	require.True(t, completed.ArtifactAvailable)

	artifactURL := testServer.URL + "/backup/v2/exports/" + created.JobID + "/artifact"
	head, err := http.NewRequestWithContext(t.Context(), http.MethodHead, artifactURL, nil)
	require.NoError(t, err)
	head.SetBasicAuth("operator", "secret")
	response, err = testServer.Client().Do(head)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, backupfmt.MediaType, response.Header.Get("Content-Type"))
	require.Equal(t, `"`+completed.ArtifactSHA256+`"`, response.Header.Get("ETag"))
	require.NotEmpty(t, response.Header.Get("Content-Length"))
	require.NoError(t, response.Body.Close())

	rangeRequest, err := http.NewRequestWithContext(t.Context(), http.MethodGet, artifactURL, nil)
	require.NoError(t, err)
	rangeRequest.SetBasicAuth("operator", "secret")
	rangeRequest.Header.Set("Range", "bytes=0-9")
	response, err = testServer.Client().Do(rangeRequest)
	require.NoError(t, err)
	require.Equal(t, http.StatusPartialContent, response.StatusCode)
	raw, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Len(t, raw, 10)

	fullArtifactRequest, err := http.NewRequestWithContext(
		t.Context(),
		http.MethodGet,
		artifactURL,
		nil,
	)
	require.NoError(t, err)
	fullArtifactRequest.SetBasicAuth("operator", "secret")
	response, err = testServer.Client().Do(fullArtifactRequest)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	artifactRaw, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())

	importRequest, err := http.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		testServer.URL+"/backup/v2/imports?conflict=replace&dry_run=true",
		bytes.NewReader(artifactRaw),
	)
	require.NoError(t, err)
	importRequest.SetBasicAuth("operator", "secret")
	importRequest.Header.Set("Content-Type", backupfmt.MediaType)
	importRequest.Header.Set("Idempotency-Key", "http-import-dry-run")
	importRequest.Header.Set("X-Tgfile-Artifact-Sha256", completed.ArtifactSHA256)
	response, err = testServer.Client().Do(importRequest)
	require.NoError(t, err)
	require.Equal(t, http.StatusAccepted, response.StatusCode)
	var dryRun backupmgr.Job
	require.NoError(t, json.NewDecoder(response.Body).Decode(&dryRun))
	require.NoError(t, response.Body.Close())
	require.Eventually(t, func() bool {
		job, getErr := manager.GetJob(t.Context(), dryRun.JobID)
		return getErr == nil && job.State == "succeeded"
	}, 5*time.Second, 25*time.Millisecond)

	metricsRequest, err := http.NewRequestWithContext(
		t.Context(),
		http.MethodGet,
		testServer.URL+"/backup/v2/metrics",
		nil,
	)
	require.NoError(t, err)
	metricsRequest.SetBasicAuth("operator", "secret")
	response, err = testServer.Client().Do(metricsRequest)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	metrics, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Contains(t, string(metrics), `tgfile_backup_jobs_total{kind="export",result="succeeded"} 1`)
	require.Contains(t, string(metrics), "tgfile_backup_artifact_bytes ")
	require.NotContains(t, string(metrics), created.JobID)

	foreign, err := http.NewRequestWithContext(
		t.Context(),
		http.MethodGet,
		testServer.URL+"/backup/v2/jobs/"+created.JobID,
		nil,
	)
	require.NoError(t, err)
	foreign.SetBasicAuth("reader", "secret")
	response, err = testServer.Client().Do(foreign)
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	require.NoError(t, response.Body.Close())

	legacy, err := http.NewRequestWithContext(
		t.Context(),
		http.MethodGet,
		testServer.URL+"/backup/export",
		nil,
	)
	require.NoError(t, err)
	legacy.SetBasicAuth("operator", "secret")
	response, err = testServer.Client().Do(legacy)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, response.StatusCode)
	require.NoError(t, response.Body.Close())
}
