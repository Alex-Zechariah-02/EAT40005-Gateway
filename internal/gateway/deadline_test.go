// Deadline predicate, timeout without traffic and clock lead.

package gateway

import (
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/compile"
	"github.com/Alex-Zechariah-02/apiward/internal/store"
)

// These tests cover the confirmation deadline: the clock rule itself, the
// automatic rollback of a running gateway without any traffic, a deadline set
// while the wall clock was ahead, a late or missing timer under traffic, the
// decision instant at the boundary, and restarts before and after the
// deadline.

// waitFor polls cond until it holds or the limit passes.
func waitFor(t *testing.T, limit time.Time, what string, cond func() bool) {
	t.Helper()
	for !cond() {
		if time.Now().After(limit) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// A provisional generation expires at its deadline on whichever clock
// reaches it first; a confirmed generation never expires. The two clocks are
// set apart here as a wall clock step or a paused monotonic clock would.
// (The test gateway can also move its wall clock alone: see
// TestOverrideExpiryUnderClockChanges.)
func TestDeadlinePredicate(t *testing.T) {
	deadline := time.UnixMilli(time.Now().Add(time.Hour).UnixMilli())
	g, err := compile.Compile(threeAPIPolicy(), 2, deadline)
	if err != nil {
		t.Fatal(err)
	}
	if !g.NotAfter.Equal(deadline) {
		t.Fatalf("NotAfter %v, want %v", g.NotAfter, deadline)
	}
	wall := []struct {
		at      time.Time
		expired bool
	}{
		{deadline.Add(-time.Millisecond), false},
		{deadline, true},
		{deadline.Add(time.Millisecond), true},
	}
	for _, w := range wall {
		if got := g.Expired(w.at); got != w.expired {
			t.Errorf("wall clock %v: expired %v, want %v", w.at, got, w.expired)
		}
	}
	if g.Expired(compile.TrustedNow()) {
		t.Fatal("expired an hour before its deadline")
	}
	if !g.Expired(compile.TrustedNow().Add(time.Hour + time.Second)) {
		t.Fatal("not expired a second after its deadline on both clocks")
	}

	// The trusted time is the later of the wall clock and the monotonic
	// clock's estimate of it, measured from the process start.
	start := compile.ProcessStart.Round(0)
	// The wall clock was set back an hour, but by the monotonic clock the
	// deadline has passed.
	monotonicDeadline := start.Add(2 * time.Hour)
	setBack := &compile.Generation{NotAfter: monotonicDeadline}
	if now := compile.TrustedTime(monotonicDeadline.Add(-time.Hour), 2*time.Hour); !setBack.Expired(now) || !now.Equal(monotonicDeadline) {
		t.Fatalf("a wall clock set back extended the deadline (trusted time %v)", now)
	}
	// The monotonic clock paused (system suspend) while the wall clock moved
	// past the deadline.
	wallDeadline := start.Add(3 * time.Hour)
	suspended := &compile.Generation{NotAfter: wallDeadline}
	if now := compile.TrustedTime(wallDeadline.Add(time.Millisecond), time.Hour); !suspended.Expired(now) || !now.Equal(wallDeadline.Add(time.Millisecond)) {
		t.Fatalf("a paused monotonic clock extended the deadline (trusted time %v)", now)
	}
	// Neither clock has reached the deadline.
	if now := compile.TrustedTime(wallDeadline.Add(-time.Minute), time.Hour); suspended.Expired(now) {
		t.Fatalf("expired before either clock reached the deadline (trusted time %v)", now)
	}
	confirmed, err := compile.Compile(threeAPIPolicy(), 2, time.Time{})
	if err != nil || confirmed.Expired(compile.TrustedNow().Add(1000*time.Hour)) {
		t.Fatalf("a confirmed generation expired: %v", err)
	}
}

// With no request at all, the controller rolls back an expired
// provisional base on its own. A timer that fires after a confirmation or an
// explicit rollback changes nothing.
func TestTimeoutWithoutTraffic(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.plain)
	harness := e.mustLogin(harnessAdmin, harnessPassword)
	baseGeneration := e.status().Generation

	// Only Admin API status reads happen while the deadline passes.
	_, expiring := e.provisionalFrom(harness, e.cpaCandidate(), 1, 2, "quiet-timeout-01")
	waitFor(t, expiring.Deadline.Add(5*time.Second), "the automatic rollback", func() bool { return e.status().Provisional == 0 })
	s := e.status()
	if s.Revision != 1 || s.Confirmed != 1 || s.Generation != baseGeneration || !s.NotAfter.IsZero() {
		t.Fatalf("status after the timeout: %+v", s)
	}
	e.expectEnforced(false, baseGeneration)
	code, _, body := e.resolve(harness, "confirm", 2, `"r1"`, "confirm-after-timeout")
	if outcome := fromJSON[Outcome](t, body); code != http.StatusConflict || outcome.Reason != "ALREADY_RESOLVED" || outcome.Disposition != store.DispositionTimedOut {
		t.Fatalf("confirmation after the timeout: %d %s", code, body)
	}

	// Confirmed before its deadline: the timer must not undo it.
	_, kept := e.provisionalFrom(harness, e.cpaCandidate(), 1, 2, "quiet-timeout-02")
	if code, _, body := e.resolve(harness, "confirm", kept.Provisional, `"r1"`, "quiet-timeout-02-confirm"); code != http.StatusOK {
		t.Fatalf("confirmation: %d %s", code, body)
	}
	// Rolled back before its deadline: the timer must not act again.
	_, withdrawn := e.provisionalFrom(harness, e.protectedPolicy(), kept.Provisional, 2, "quiet-timeout-03")
	if code, _, body := e.resolve(harness, "rollback", withdrawn.Provisional, policyETag(kept.Provisional), "quiet-timeout-03-rollback"); code != http.StatusOK {
		t.Fatalf("rollback: %d %s", code, body)
	}
	time.Sleep(time.Until(withdrawn.Deadline.Add(1500 * time.Millisecond)))
	if s := e.status(); s.Confirmed != kept.Provisional || s.Provisional != 0 || s.Revision != kept.Provisional {
		t.Fatalf("status after the stale deadlines: %+v", s)
	}
	e.expectEnforced(true, "")

	events := e.eventsWhen(func(events []map[string]any) bool { return countKind(events, "cpa.timed_out") > 0 })
	if n := countKind(events, "cpa.timed_out"); n != 1 {
		t.Fatalf("%d timeout events, want 1", n)
	}
	ev := findTransition(events, "cpa.timed_out", "auto-rollback:2")
	if ev == nil || ev["actor"] != "controller" || ev["revision"] != float64(1) || ev["provisional"] != float64(2) {
		t.Fatalf("timeout event %v", ev)
	}
	e.stop()
	list := storedActivations(t, e.database)
	want := []string{store.DispositionTimedOut, store.DispositionConfirmed, store.DispositionRolledBack}
	if len(list) != len(want) {
		t.Fatalf("stored activations %+v", list)
	}
	for i, a := range list {
		if a.Disposition != want[i] {
			t.Fatalf("activation %d disposition %q, want %q", a.Revision, a.Disposition, want[i])
		}
	}
	timedOut := list[0]
	if timedOut.ResolvedBy != "controller" || timedOut.Resolution != "auto-rollback:2" || timedOut.ResolvedAt.Before(timedOut.Deadline) ||
		timedOut.ResolvedAt.Sub(timedOut.Deadline) > time.Second {
		t.Fatalf("timed-out activation %+v: resolved %v after its deadline", timedOut, timedOut.ResolvedAt.Sub(timedOut.Deadline))
	}
	if n := queryInt(t, e.database, `SELECT COUNT(*) FROM transition WHERE id LIKE 'auto-rollback:%'`); n != 1 {
		t.Fatalf("%d automatic rollbacks, want 1", n)
	}
}

// A provisional base activated while the wall clock is an hour ahead, with
// the clock then set back, still rolls back after its own two seconds rather
// than an hour later.
func TestDeadlineSetWhileClockAhead(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	offset := filepath.Join(t.TempDir(), "wall-offset")
	setOffset := func(d time.Duration) {
		if err := os.WriteFile(offset, []byte(strconv.FormatInt(d.Milliseconds(), 10)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	setOffset(0)
	e.start(binaries.fault, "GATEWAY_WALL_OFFSET="+offset)
	harness := e.mustLogin(harnessAdmin, harnessPassword)
	baseGeneration := e.status().Generation

	setOffset(time.Hour)
	_, ahead := e.provisionalFrom(harness, e.cpaCandidate(), 1, 2, "clock-ahead-cpa-01")
	setOffset(0)
	e.expectEnforced(true, ahead.Generation)
	waitFor(t, time.Now().Add(5*time.Second), "the rollback after the clock moved back", func() bool { return e.status().Provisional == 0 })
	e.expectEnforced(false, baseGeneration)
}
