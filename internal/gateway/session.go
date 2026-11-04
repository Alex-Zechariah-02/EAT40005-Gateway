// Admin sessions and session tokens.

package gateway

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

// newToken returns 32 random bytes in URL-safe base64.
func newToken() string {
	token := make([]byte, 32)
	rand.Read(token)
	return base64.RawURLEncoding.EncodeToString(token)
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

const maxSessions = 64

var errSessionsFull = errors.New("session limit reached")

// session is one authenticated administrator session. The server keeps only
// the SHA-256 of the session token; the token itself exists only in the
// administrator's cookie.
type session struct {
	admin      string
	credential int64 // administrator credential version at login
	csrf       string
	reference  string // non-secret identifier recorded in audit events
	lastSeen   time.Time
	expires    time.Time     // absolute end of the session
	done       chan struct{} // closes when revoked or swept; long-lived readers stop too
}

// sessions holds the live administrator sessions in memory. A restart ends
// every session; nothing about them is ever written to the database.
type sessions struct {
	mu       sync.Mutex
	byHash   map[string]*session
	idle     time.Duration
	lifetime time.Duration
}

func newSessions(idle, lifetime time.Duration) *sessions {
	return &sessions{byHash: map[string]*session{}, idle: idle, lifetime: lifetime}
}

// create starts a session and returns its token. Expired sessions are swept
// first; if the limit is still reached, no session is created.
func (s *sessions) create(admin string, credential int64, now time.Time) (string, session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for hash, existing := range s.byHash {
		if !s.alive(existing, now) {
			s.end(hash)
		}
	}
	if len(s.byHash) >= maxSessions {
		return "", session{}, errSessionsFull
	}
	token := newToken()
	hash := tokenHash(token)
	created := &session{
		admin:      admin,
		credential: credential,
		csrf:       newToken(),
		reference:  hash[:16],
		lastSeen:   now,
		expires:    now.Add(s.lifetime),
		done:       make(chan struct{}),
	}
	s.byHash[hash] = created
	return token, *created, nil
}

func (s *sessions) alive(entry *session, now time.Time) bool {
	return now.Before(entry.expires) && now.Before(entry.lastSeen.Add(s.idle))
}

// lookup returns the session for a token and records the activity. An unknown
// token is never adopted; an expired one is removed.
func (s *sessions) lookup(token string, now time.Time) (session, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	hash := tokenHash(token)
	entry := s.byHash[hash]
	if entry == nil {
		return session{}, "no_session"
	}
	if !s.alive(entry, now) {
		s.end(hash)
		return session{}, "expired"
	}
	entry.lastSeen = now
	return *entry, ""
}

func (s *sessions) revoke(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.end(tokenHash(token))
}

// end is called with s.mu held. Removing the entry makes closure one-shot.
func (s *sessions) end(hash string) {
	if entry := s.byHash[hash]; entry != nil {
		close(entry.done)
		delete(s.byHash, hash)
	}
}

func (s *sessions) active(token string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.byHash[tokenHash(token)]
	return entry != nil && s.alive(entry, now)
}

// revokeAdmin ends every session of one administrator, as a credential change
// requires.
func (s *sessions) revokeAdmin(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for hash, entry := range s.byHash {
		if entry.admin == name {
			s.end(hash)
		}
	}
}
