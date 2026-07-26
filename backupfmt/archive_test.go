package backupfmt

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBuildInspectAndVerify(t *testing.T) {
	t.Parallel()
	manifest := testManifest()
	filename := filepath.Join(t.TempDir(), "backup.tgfb")
	output, err := os.OpenFile(filename, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	err = Build(t.Context(), output, &manifest, testLimits(), func(
		_ context.Context,
		fileRef string,
		partIndex int,
	) (io.ReadCloser, error) {
		require.Equal(t, "f00000001", fileRef)
		require.Zero(t, partIndex)
		return io.NopCloser(bytes.NewReader([]byte("hello world"))), nil
	})
	require.NoError(t, errors.Join(err, output.Close()))

	inspected, inspectReport, err := InspectFile(t.Context(), filename, testLimits(), 20*1024*1024)
	require.NoError(t, err)
	require.Equal(t, manifest, *inspected)
	require.NotEmpty(t, inspectReport.ArtifactSHA256)
	require.Positive(t, inspectReport.ArtifactBytes)

	verified, verifyReport, err := VerifyFile(t.Context(), filename, testLimits(), 20*1024*1024)
	require.NoError(t, err)
	require.Equal(t, inspected, verified)
	require.Equal(t, inspectReport, verifyReport)
}

func TestBuildRejectsShortAndLongParts(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		content string
	}{
		{name: "short", content: "short"},
		{name: "long", content: "hello world!"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			manifest := testManifest()
			var output bytes.Buffer
			err := Build(t.Context(), &output, &manifest, testLimits(), func(
				context.Context,
				string,
				int,
			) (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader([]byte(test.content))), nil
			})
			require.Error(t, err)
		})
	}
}

func TestVerifyRejectsTrailingData(t *testing.T) {
	t.Parallel()
	manifest := testManifest()
	filename := filepath.Join(t.TempDir(), "backup.tgfb")
	output, err := os.Create(filename)
	require.NoError(t, err)
	require.NoError(t, Build(t.Context(), output, &manifest, testLimits(), func(
		context.Context,
		string,
		int,
	) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader([]byte("hello world"))), nil
	}))
	require.NoError(t, output.Close())
	output, err = os.OpenFile(filename, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = output.WriteString("trailing")
	require.NoError(t, errors.Join(err, output.Close()))

	_, _, err = VerifyFile(t.Context(), filename, testLimits(), 20*1024*1024)
	require.ErrorIs(t, err, ErrInvalidArchive)
}

func TestStrictJSONRejectsDuplicateAndUnknownFields(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		`{"format":"tgfile-logical-backup","format":"tgfile-logical-backup","version":2}`,
		`{"format":"tgfile-logical-backup","version":2,"unknown":true}`,
		`{"format":"tgfile-logical-backup","version":2} {}`,
	} {
		var header FormatHeader
		require.Error(t, decodeStrictJSON([]byte(raw), &header))
	}
	var header FormatHeader
	require.Error(t, decodeStrictJSON([]byte{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'}, &header))
}

func TestValidateManifestBoundaries(t *testing.T) {
	t.Parallel()
	t.Run("wrong summary", func(t *testing.T) {
		t.Parallel()
		manifest := testManifest()
		manifest.Limits.PartCount = 2
		require.ErrorIs(
			t,
			ValidateManifest(&manifest, testLimits(), 20*1024*1024),
			ErrInvalidArchive,
		)
	})
	t.Run("part too large", func(t *testing.T) {
		t.Parallel()
		manifest := testManifest()
		require.ErrorIs(t, ValidateManifest(&manifest, testLimits(), 10), ErrInvalidArchive)
	})
	t.Run("empty file md5", func(t *testing.T) {
		t.Parallel()
		manifest := testManifest()
		manifest.Files = []File{{
			Ref:              "f00000001",
			SourceFileID:     "2",
			LayoutVersion:    1,
			CompatibilityMD5: emptyMD5,
		}}
		manifest.Mappings[0].Size = 0
		manifest.Limits.PartCount = 0
		manifest.Limits.PhysicalBytes = 0
		require.NoError(t, ValidateManifest(&manifest, testLimits(), 20*1024*1024))
	})
	t.Run("inconsistent compatibility md5", func(t *testing.T) {
		t.Parallel()
		manifest := testManifest()
		manifest.Files[0].CompatibilityMD5 = emptyMD5
		require.ErrorIs(
			t,
			ValidateManifest(&manifest, testLimits(), 20*1024*1024),
			ErrChecksum,
		)
	})
	t.Run("source part limit", func(t *testing.T) {
		t.Parallel()
		manifest := testManifest()
		manifest.Source.MaxPartSize = 10
		require.ErrorIs(
			t,
			ValidateManifest(&manifest, testLimits(), 20*1024*1024),
			ErrInvalidArchive,
		)
	})
	t.Run("scope escape", func(t *testing.T) {
		t.Parallel()
		manifest := testManifest()
		manifest.Scope = "/other"
		require.ErrorIs(
			t,
			ValidateManifest(&manifest, testLimits(), 20*1024*1024),
			ErrInvalidArchive,
		)
	})
	t.Run("missing bucket metadata", func(t *testing.T) {
		t.Parallel()
		manifest := testManifest()
		manifest.S3Objects = nil
		require.ErrorIs(
			t,
			ValidateManifest(&manifest, testLimits(), 20*1024*1024),
			ErrInvalidArchive,
		)
	})
	t.Run("unsafe S3 header", func(t *testing.T) {
		t.Parallel()
		manifest := testManifest()
		manifest.S3Objects[0].ETag = "\"safe\"\r\nInjected: yes"
		require.ErrorIs(
			t,
			ValidateManifest(&manifest, testLimits(), 20*1024*1024),
			ErrInvalidArchive,
		)
	})
	t.Run("invalid WebDAV XML", func(t *testing.T) {
		t.Parallel()
		manifest := testManifest()
		manifest.WebDAVProperties = []WebDAVProperty{{
			Path:         "/bucket/hello.txt",
			NamespaceURI: "urn:test",
			LocalName:    "color",
			ValueXML:     "<broken>",
		}}
		require.ErrorIs(
			t,
			ValidateManifest(&manifest, testLimits(), 20*1024*1024),
			ErrInvalidArchive,
		)
	})
	t.Run("invalid mode", func(t *testing.T) {
		t.Parallel()
		manifest := testManifest()
		manifest.Mappings[0].Mode = 1 << 31
		require.ErrorIs(
			t,
			ValidateManifest(&manifest, testLimits(), 20*1024*1024),
			ErrInvalidArchive,
		)
	})
}

func TestValidateCompletedChecksum(t *testing.T) {
	t.Parallel()
	require.NoError(t, validateCompletedChecksum(CompletedPart{
		ChecksumState:     "available",
		ChecksumAlgorithm: "CRC32",
		ChecksumValue:     "AAAAAA==",
	}))
	require.ErrorIs(t, validateCompletedChecksum(CompletedPart{
		ChecksumState:     "available",
		ChecksumAlgorithm: "CRC32",
		ChecksumValue:     "not-base64",
	}), ErrInvalidArchive)
	require.ErrorIs(t, validateCompletedChecksum(CompletedPart{
		ChecksumState:     "available",
		ChecksumAlgorithm: "MD5",
		ChecksumValue:     "1B2M2Y8AsgTpgAmY7PhCfg==",
	}), ErrInvalidArchive)
	require.ErrorIs(t, validateCompletedPartContent([]File{{
		Ref:           "f00000002",
		LayoutVersion: 2,
		Segments: []Segment{{
			Index: 0, SourceRef: "f00000001", Size: 0,
		}},
		CompletedParts: []CompletedPart{{
			PartNumber:        1,
			ChecksumState:     "available",
			ChecksumAlgorithm: "CRC32",
			ChecksumValue:     "AQAAAA==",
		}},
	}}, map[string]fileContentDigest{
		"f00000001": emptyContentDigest("f00000001"),
	}), ErrChecksum)
}

func testManifest() Manifest {
	return Manifest{
		Format:    FormatName,
		Version:   FormatVersion,
		CreatedAt: time.Unix(1, 0).UTC().Format(time.RFC3339),
		Scope:     "/",
		Source: Source{
			SchemaVersion: 12,
			BlockIOKind:   "telegram",
			MaxPartSize:   20 * 1024 * 1024,
		},
		Limits: Summary{
			MappingCount:   1,
			DirectoryCount: 1,
			FileCount:      1,
			PartCount:      1,
			PhysicalBytes:  11,
		},
		RequiredBuckets: []RequiredBucket{{Name: "bucket", ACL: "private"}},
		Files: []File{{
			Ref:              "f00000001",
			SourceFileID:     "1",
			LayoutVersion:    1,
			Size:             11,
			CompatibilityMD5: "5eb63bbbe01eeed093cb22bb8f5acdc3",
			Parts: []Part{{
				Index:  0,
				Size:   11,
				MD5:    "5eb63bbbe01eeed093cb22bb8f5acdc3",
				SHA256: "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9",
				Entry:  "parts/f00000001/00000000.bin",
			}},
		}},
		Directories: []Directory{{Path: "/bucket", Mode: 0o755}},
		Mappings: []Mapping{{
			Path:    "/bucket/hello.txt",
			FileRef: "f00000001",
			Size:    11,
			Mode:    0o644,
		}},
		S3Objects: []S3Object{{
			Path:         "/bucket/hello.txt",
			ETag:         `"5eb63bbbe01eeed093cb22bb8f5acdc3"`,
			UserMetadata: "{}",
		}},
	}
}

func testLimits() Limits {
	limits := DefaultLimits()
	limits.MaxArchiveBytes = 1024 * 1024
	limits.MaxExpandedBytes = 1024 * 1024
	limits.MaxManifestBytes = 1024 * 1024
	return limits
}
