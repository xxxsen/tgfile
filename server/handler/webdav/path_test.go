package webdav

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPathWithinRootUsesSegmentBoundary(t *testing.T) {
	assert.True(t, pathWithinRoot("/data", "/data"))
	assert.True(t, pathWithinRoot("/data", "/data/files/item"))
	assert.False(t, pathWithinRoot("/data", "/database/item"))
	assert.False(t, pathWithinRoot("/data", "/other/item"))
	assert.True(t, pathWithinRoot("/", "/any/item"))
}

func TestDAVIfParserSupportsMultipleTaggedResources(t *testing.T) {
	header, err := (&davIfParser{
		value: "</webdav/source> (<source-token>) ([\"source-etag\"]) " +
			"</webdav/destination> (Not <destination-token>)",
	}).parse()
	require.NoError(t, err)
	require.Len(t, header.Lists, 3)
	assert.Equal(t, "/webdav/source", header.Lists[0].Resource)
	assert.Equal(t, "/webdav/source", header.Lists[1].Resource)
	assert.Equal(t, "/webdav/destination", header.Lists[2].Resource)
	assert.Equal(t, "source-token", header.Lists[0].Terms[0].LockToken)
	assert.Equal(t, `"source-etag"`, header.Lists[1].Terms[0].ETag)
	assert.True(t, header.Lists[2].Terms[0].Not)

	_, err = (&davIfParser{
		value: "(<untagged>) </webdav/tagged> (<tagged>)",
	}).parse()
	require.ErrorIs(t, err, errInvalidIfHeader)
}

func TestParseLockTimeoutCapsBeforeDurationConversion(t *testing.T) {
	timeout, err := parseLockTimeout("Second-9223372036854775807")
	require.NoError(t, err)
	assert.Equal(t, maxLockTimeout, timeout)
}

func TestSameOriginNormalizesDefaultPorts(t *testing.T) {
	assert.True(t, sameOrigin(
		&url.URL{Scheme: "https", Host: "files.example.test:443"},
		&url.URL{Scheme: "HTTPS", Host: "FILES.EXAMPLE.TEST"},
	))
	assert.False(t, sameOrigin(
		&url.URL{Scheme: "https", Host: "files.example.test:8443"},
		&url.URL{Scheme: "https", Host: "files.example.test"},
	))
}
