package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"sync"
	"time"
)

// Session is a login of one browser.
type Session struct {
	User     string
	Created  time.Time
	LastSeen time.Time
}

// Sessions are kept in memory: a restart logs everybody out, which is acceptable for
// a tool like this and leaves no token on disk.
type Sessions struct {
	mu       sync.Mutex
	byHash   map[string]*Session
	idle     time.Duration
	absolute time.Duration
	now      func() time.Time
}

// NewSessions creates a store. A session ends after idle without a request, and
// after absolute in any case.
func NewSessions(idle, absolute time.Duration, now func() time.Time) *Sessions {
	return &Sessions{byHash: map[string]*Session{}, idle: idle, absolute: absolute, now: now}
}

// Only a hash of a token is kept, so that a dump of the memory holds no valid token.
func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Create starts a session and returns its token.
func (s *Sessions) Create(user string) (string, error) {
	token, err := RandomToken()
	if err != nil {
		return "", err
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune(now)
	s.limit(user)
	s.byHash[tokenHash(token)] = &Session{User: user, Created: now, LastSeen: now}
	return token, nil
}

// maxSessionsPerUser bounds the sessions of one account; a new one ends the oldest.
const maxSessionsPerUser = 20

func (s *Sessions) limit(user string) {
	var mine []string
	for hash, session := range s.byHash {
		if strings.EqualFold(session.User, user) {
			mine = append(mine, hash)
		}
	}
	if len(mine) < maxSessionsPerUser {
		return
	}
	slices.SortFunc(mine, func(a, b string) int { return s.byHash[a].Created.Compare(s.byHash[b].Created) })
	for _, hash := range mine[:len(mine)-maxSessionsPerUser+1] {
		delete(s.byHash, hash)
	}
}

func (s *Sessions) expired(session *Session, now time.Time) bool {
	return now.Sub(session.LastSeen) > s.idle || now.Sub(session.Created) > s.absolute
}

func (s *Sessions) prune(now time.Time) {
	for hash, session := range s.byHash {
		if s.expired(session, now) {
			delete(s.byHash, hash)
		}
	}
}

// Get returns the user of a valid token and keeps the session alive.
func (s *Sessions) Get(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	hash := tokenHash(token)
	session, ok := s.byHash[hash]
	if !ok {
		return "", false
	}
	if s.expired(session, now) {
		delete(s.byHash, hash)
		return "", false
	}
	session.LastSeen = now
	return session.User, true
}

// Delete ends one session.
func (s *Sessions) Delete(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byHash, tokenHash(token))
}

// DeleteUser ends every session of a user, after a new password or a removal.
func (s *Sessions) DeleteUser(user string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for hash, session := range s.byHash {
		if strings.EqualFold(session.User, user) {
			delete(s.byHash, hash)
		}
	}
}
