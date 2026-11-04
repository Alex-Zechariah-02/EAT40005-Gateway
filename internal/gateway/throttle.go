// Sign-in failure throttling.

package gateway

import (
	"sync"
	"time"
)

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
