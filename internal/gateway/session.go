package gateway

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// permission is a set of administrator rights. Each admin route requires
// exactly one; holding read never implies the right to change anything.
type permission int

const (
	permRead permission = 1 << iota
	permReadHistory
	permManageSubjects
	permManagePolicy

	permAll = permRead | permReadHistory | permManageSubjects | permManagePolicy
)

var permissionNames = []struct {
	bit  permission
	name string
}{
	{permRead, "read"},
	{permReadHistory, "read_history"},
	{permManageSubjects, "manage_subjects"},
	{permManagePolicy, "manage_policy"},
}

func (p permission) names() []string {
	names := []string{}
	for _, entry := range permissionNames {
		if p&entry.bit != 0 {
			names = append(names, entry.name)
		}
	}
	return names
}

// parsePermissions reads a comma-separated list of permission names.
func parsePermissions(list string) (permission, error) {
	var p permission
	for name := range strings.SplitSeq(list, ",") {
		found := false
		for _, entry := range permissionNames {
			if strings.TrimSpace(name) == entry.name {
				p |= entry.bit
				found = true
			}
		}
		if !found {
			return 0, fmt.Errorf("unknown permission %q", name)
		}
	}
	return p, nil
}

// Administrator names are short lowercase identifiers, safe to log.
func isAdminName(name string) bool {
	if name == "" || len(name) > maxIDLength {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		alphanumeric := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
		if alphanumeric {
			continue
		}
		if i == 0 || (c != '.' && c != '_' && c != '-') {
			return false
		}
	}
	return true
}

// Password hashing uses argon2id with the OWASP minimum configuration. Only
// hashes with exactly these parameters are accepted, so a stored value can
// never make the gateway compute an attacker-chosen cost.
const (
	argonMemory      = 19 * 1024 // KiB
	argonTime        = 2
	argonThreads     = 1
	argonSaltBytes   = 16
	argonKeyBytes    = 32
	minPasswordBytes = 15
	maxPasswordBytes = 256
)

var argonPrefix = fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$", argon2.Version, argonMemory, argonTime, argonThreads)

func validPassword(password string) bool {
	return len(password) >= minPasswordBytes && len(password) <= maxPasswordBytes && utf8.ValidString(password)
}

func hashPassword(password string) string {
	salt := make([]byte, argonSaltBytes)
	rand.Read(salt)
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyBytes)
	encoding := base64.RawStdEncoding
	return argonPrefix + encoding.EncodeToString(salt) + "$" + encoding.EncodeToString(key)
}

func verifyPassword(encoded, password string) bool {
	rest, ok := strings.CutPrefix(encoded, argonPrefix)
	if !ok {
		return false
	}
	saltText, keyText, ok := strings.Cut(rest, "$")
	if !ok {
		return false
	}
	salt, saltErr := base64.RawStdEncoding.DecodeString(saltText)
	key, keyErr := base64.RawStdEncoding.DecodeString(keyText)
	if saltErr != nil || keyErr != nil || len(salt) != argonSaltBytes || len(key) != argonKeyBytes {
		return false
	}
	computed := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyBytes)
	return subtle.ConstantTimeCompare(computed, key) == 1
}

// dummyHash is verified against when the named administrator does not exist,
// so the response time does not reveal which names exist.
var dummyHash = sync.OnceValue(func() string { return hashPassword(newToken()) })

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
	created    time.Time
	lastSeen   time.Time
	expires    time.Time // absolute end of the session
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
			delete(s.byHash, hash)
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
		created:    now,
		lastSeen:   now,
		expires:    now.Add(s.lifetime),
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
		delete(s.byHash, hash)
		return session{}, "expired"
	}
	entry.lastSeen = now
	return *entry, ""
}

func (s *sessions) revoke(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byHash, tokenHash(token))
}

// revokeAdmin ends every session of one administrator, as a credential change
// requires.
func (s *sessions) revokeAdmin(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for hash, entry := range s.byHash {
		if entry.admin == name {
			delete(s.byHash, hash)
		}
	}
}

// Failed-attempt limits for logins and enrolments.
const (
	maxNameFailures   = 5
	lockoutPeriod     = 15 * time.Minute
	failureWindow     = 15 * time.Minute
	maxWindowFailures = 50
	maxHashing        = 2
)

// throttle limits password guessing. Each existing administrator is locked
// after maxNameFailures consecutive failures, even against the correct
// password, and all attempts stop once maxWindowFailures failures occurred in
// the current window. Counters are in memory and only a restart clears them.
type throttle struct {
	mu          sync.Mutex
	names       map[string]*nameFailures
	windowStart time.Time
	failures    int
}

type nameFailures struct {
	count       int
	lockedUntil time.Time
}

func newThrottle() *throttle { return &throttle{names: map[string]*nameFailures{}} }

// check returns the refusal reason for an attempt on the name, if any.
func (t *throttle) check(name string, now time.Time) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if now.Sub(t.windowStart) >= failureWindow {
		t.windowStart, t.failures = now, 0
	}
	if t.failures >= maxWindowFailures {
		return "throttled"
	}
	if entry := t.names[name]; entry != nil && now.Before(entry.lockedUntil) {
		return "locked"
	}
	return ""
}

// failed counts a failed attempt. Only names of existing administrators get
// their own counter, so unknown names cannot grow the table.
func (t *throttle) failed(name string, known bool, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.failures++
	if !known {
		return
	}
	entry := t.names[name]
	if entry == nil {
		entry = &nameFailures{}
		t.names[name] = entry
	}
	entry.count++
	if entry.count >= maxNameFailures {
		entry.count = 0
		entry.lockedUntil = now.Add(lockoutPeriod)
	}
}

func (t *throttle) succeeded(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.names, name)
}
