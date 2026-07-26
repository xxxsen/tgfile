package admin

import (
	"crypto/sha256"
	"sync"
	"time"
)

const (
	maxLoginLimiterKeys = 4096
	maxLoginFailures    = 5
	maxGlobalLogins     = 120
	loginWindow         = 5 * time.Minute
	globalLoginWindow   = time.Minute
)

type loginFailure struct {
	attempts []time.Time
}

type loginLimiter struct {
	mu       sync.Mutex
	failures map[[sha256.Size]byte]*loginFailure
	global   []time.Time
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{failures: make(map[[sha256.Size]byte]*loginFailure)}
}

func (l *loginLimiter) begin(peer, username string, now time.Time) ([sha256.Size]byte, bool) {
	key := sha256.Sum256([]byte(peer + "\x00" + username))
	l.mu.Lock()
	defer l.mu.Unlock()
	l.global = trimTimes(l.global, now.Add(-globalLoginWindow))
	if len(l.global) >= maxGlobalLogins {
		return key, false
	}
	l.global = append(l.global, now)
	record, exists := l.failures[key]
	if !exists {
		if len(l.failures) >= maxLoginLimiterKeys {
			l.cleanupLocked(now)
			if len(l.failures) >= maxLoginLimiterKeys {
				return key, false
			}
		}
		return key, true
	}
	record.attempts = trimTimes(record.attempts, now.Add(-loginWindow))
	if len(record.attempts) == 0 {
		delete(l.failures, key)
		return key, true
	}
	return key, len(record.attempts) < maxLoginFailures
}

func (l *loginLimiter) fail(key [sha256.Size]byte, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	record := l.failures[key]
	if record == nil {
		record = &loginFailure{}
		l.failures[key] = record
	}
	record.attempts = append(trimTimes(record.attempts, now.Add(-loginWindow)), now)
}

func (l *loginLimiter) success(key [sha256.Size]byte) {
	l.mu.Lock()
	delete(l.failures, key)
	l.mu.Unlock()
}

func (l *loginLimiter) cleanupLocked(now time.Time) {
	for key, record := range l.failures {
		record.attempts = trimTimes(record.attempts, now.Add(-loginWindow))
		if len(record.attempts) == 0 {
			delete(l.failures, key)
		}
	}
}

func trimTimes(values []time.Time, cutoff time.Time) []time.Time {
	index := 0
	for index < len(values) && values[index].Before(cutoff) {
		index++
	}
	return values[index:]
}
