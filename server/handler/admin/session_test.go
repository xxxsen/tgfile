package admin

import (
	"encoding/base64"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xxxsen/tgfile/filemgr"
)

func TestSessionStoreSlidingIdleAbsoluteExpiryAndPerUserLimit(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	store := newSessionStore(10*time.Minute, time.Hour)
	store.now = func() time.Time { return now }

	firstToken, first, err := store.create("viewer", roleRead)
	require.NoError(t, err)
	require.NotEqual(t, firstToken, first.csrf)
	require.Len(t, firstToken, base64.RawURLEncoding.EncodedLen(sessionTokenBytes))
	require.NotContains(t, store.records, firstToken)

	now = now.Add(9 * time.Minute)
	sliding, ok := store.get(firstToken)
	require.True(t, ok)
	require.Equal(t, now.Add(10*time.Minute), store.idleExpiry(sliding))

	for minute := 18; minute <= 54; minute += 9 {
		now = first.createdAt.Add(time.Duration(minute) * time.Minute)
		_, ok = store.get(firstToken)
		require.True(t, ok)
	}
	now = first.createdAt.Add(59 * time.Minute)
	nearAbsolute, ok := store.get(firstToken)
	require.True(t, ok)
	require.Equal(t, first.expiresAt, store.idleExpiry(nearAbsolute))
	now = first.expiresAt
	_, ok = store.get(firstToken)
	require.False(t, ok)

	now = now.Add(time.Minute)
	tokens := make([]string, 0, maxSessionsPerUser+1)
	for range maxSessionsPerUser + 1 {
		token, _, err := store.create("operator", roleReadWrite)
		require.NoError(t, err)
		tokens = append(tokens, token)
		now = now.Add(time.Millisecond)
	}
	_, ok = store.get(tokens[0])
	require.False(t, ok)
	for _, token := range tokens[1:] {
		_, ok = store.get(token)
		require.True(t, ok)
	}
}

func TestSessionStoreHasBoundedGlobalCapacity(t *testing.T) {
	store := newSessionStore(10*time.Minute, time.Hour)
	for index := range maxSessions {
		_, _, err := store.create("user-"+strconv.Itoa(index), roleRead)
		require.NoError(t, err)
	}
	_, _, err := store.create("overflow", roleRead)
	require.ErrorIs(t, err, errSessionCapacity)
}

func TestAdminCursorAndDispositionEncodingAreStrict(t *testing.T) {
	cursor, err := encodeEntryCursor(&testFileCursor)
	require.NoError(t, err)
	decoded, exists, err := decodeEntryCursor(cursor)
	require.NoError(t, err)
	require.True(t, exists)
	require.Equal(t, testFileCursor, decoded)

	raw := base64.RawURLEncoding.EncodeToString([]byte(
		`{"v":1,"parent_entry_id":1,"file_kind":2,"file_name":"x","entry_id":2,"extra":true}`,
	))
	_, _, err = decodeEntryCursor(raw)
	require.Error(t, err)
	_, _, err = decodeEntryCursor(strings.Repeat("a", 2049))
	require.Error(t, err)

	disposition := safeDisposition("报告\r\n.txt")
	require.NotContains(t, disposition, "\r")
	require.NotContains(t, disposition, "\n")
	require.Contains(t, disposition, "filename=")
	require.Contains(t, disposition, "filename*=UTF-8''")
}

func TestAdminOriginParserRejectsAmbiguousOrigins(t *testing.T) {
	for _, value := range []string{
		"https://:443",
		"https://example.test:0",
		"https://example.test:65536",
		"https://user@example.test",
		"https://example.test/path",
	} {
		t.Run(value, func(t *testing.T) {
			_, _, err := parseOrigin(value)
			require.ErrorIs(t, err, errInvalidAdminOrigin)
		})
	}

	_, canonical, err := parseOrigin("HTTPS://EXAMPLE.TEST:443/")
	require.NoError(t, err)
	require.Equal(t, "https://example.test", canonical)
}

var testFileCursor = filemgr.FileLinkPageCursor{
	ParentEntryID: 1,
	IsDir:         false,
	Name:          "空.txt",
	EntryID:       2,
}
