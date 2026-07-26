package admin

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

const (
	sessionTokenBytes  = 32
	maxSessions        = 1024
	maxSessionsPerUser = 8
)

var errSessionCapacity = errors.New("admin session capacity is exhausted")

type sessionRecord struct {
	username  string
	role      string
	csrf      string
	createdAt time.Time
	lastSeen  time.Time
	expiresAt time.Time
}

type sessionStore struct {
	mu          sync.Mutex
	records     map[[sha256.Size]byte]*sessionRecord
	idle        time.Duration
	maximum     time.Duration
	now         func() time.Time
	lastCleanup time.Time
}

func newSessionStore(idle, maximum time.Duration) *sessionStore {
	return &sessionStore{
		records: make(map[[sha256.Size]byte]*sessionRecord),
		idle:    idle,
		maximum: maximum,
		now:     time.Now,
	}
}

func (s *sessionStore) create(username, role string) (string, *sessionRecord, error) {
	token, err := randomURLToken(sessionTokenBytes)
	if err != nil {
		return "", nil, err
	}
	csrf, err := randomURLToken(sessionTokenBytes)
	if err != nil {
		return "", nil, err
	}
	digest := sha256.Sum256([]byte(token))
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupLocked(now)
	s.revokeOldestUserSessionLocked(username)
	if len(s.records) >= maxSessions {
		return "", nil, errSessionCapacity
	}
	record := &sessionRecord{
		username:  username,
		role:      role,
		csrf:      csrf,
		createdAt: now,
		lastSeen:  now,
		expiresAt: now.Add(s.maximum),
	}
	s.records[digest] = record
	return token, cloneSession(record), nil
}

func (s *sessionStore) get(token string) (*sessionRecord, bool) {
	if !validURLToken(token, sessionTokenBytes) {
		return nil, false
	}
	digest := sha256.Sum256([]byte(token))
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupLocked(now)
	record, exists := s.records[digest]
	if !exists || s.expired(record, now) {
		delete(s.records, digest)
		return nil, false
	}
	record.lastSeen = now
	return cloneSession(record), true
}

func (s *sessionStore) delete(token string) {
	if !validURLToken(token, sessionTokenBytes) {
		return
	}
	digest := sha256.Sum256([]byte(token))
	s.mu.Lock()
	delete(s.records, digest)
	s.mu.Unlock()
}

func (s *sessionStore) cleanupLocked(now time.Time) {
	if !s.lastCleanup.IsZero() && now.Sub(s.lastCleanup) < time.Minute &&
		len(s.records) < maxSessions {
		return
	}
	for digest, record := range s.records {
		if s.expired(record, now) {
			delete(s.records, digest)
		}
	}
	s.lastCleanup = now
}

func (s *sessionStore) revokeOldestUserSessionLocked(username string) {
	count := 0
	var (
		oldestDigest [sha256.Size]byte
		oldest       *sessionRecord
	)
	for digest, record := range s.records {
		if record.username != username {
			continue
		}
		count++
		if oldest == nil || record.createdAt.Before(oldest.createdAt) {
			oldestDigest = digest
			oldest = record
		}
	}
	if count >= maxSessionsPerUser && oldest != nil {
		delete(s.records, oldestDigest)
	}
}

func (s *sessionStore) expired(record *sessionRecord, now time.Time) bool {
	return !now.Before(record.expiresAt) || !now.Before(record.lastSeen.Add(s.idle))
}

func (s *sessionStore) idleExpiry(record *sessionRecord) time.Time {
	idleExpiry := record.lastSeen.Add(s.idle)
	if record.expiresAt.Before(idleExpiry) {
		return record.expiresAt
	}
	return idleExpiry
}

func cloneSession(record *sessionRecord) *sessionRecord {
	cloned := *record
	return &cloned
}

func randomURLToken(size int) (string, error) {
	raw := make([]byte, size)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		return "", fmt.Errorf("generate admin token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func validURLToken(value string, size int) bool {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(raw) == size
}
