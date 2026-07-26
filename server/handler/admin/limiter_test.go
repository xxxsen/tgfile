package admin

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLoginLimiterPerIdentityAndGlobalWindows(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	limiter := newLoginLimiter()

	var key [32]byte
	for range maxLoginFailures {
		var allowed bool
		key, allowed = limiter.begin("127.0.0.1", "viewer", now)
		require.True(t, allowed)
		limiter.fail(key, now)
	}
	_, allowed := limiter.begin("127.0.0.1", "viewer", now)
	require.False(t, allowed)
	limiter.success(key)
	_, allowed = limiter.begin("127.0.0.1", "viewer", now)
	require.True(t, allowed)

	_, allowed = limiter.begin("127.0.0.2", "viewer", now)
	require.True(t, allowed)
	_, allowed = limiter.begin("127.0.0.1", "operator", now)
	require.True(t, allowed)

	_, allowed = limiter.begin(
		"127.0.0.1",
		"viewer",
		now.Add(loginWindow+time.Nanosecond),
	)
	require.True(t, allowed)

	global := newLoginLimiter()
	for index := range maxGlobalLogins {
		_, allowed = global.begin(
			fmt.Sprintf("192.0.2.%d", index),
			fmt.Sprintf("user-%d", index),
			now,
		)
		require.True(t, allowed)
	}
	_, allowed = global.begin("192.0.2.250", "overflow", now)
	require.False(t, allowed)
	_, allowed = global.begin(
		"192.0.2.250",
		"overflow",
		now.Add(globalLoginWindow+time.Nanosecond),
	)
	require.True(t, allowed)
}
