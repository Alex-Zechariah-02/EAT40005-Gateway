package gateway

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
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
	g, err := Compile(threeAPIPolicy(), 2, deadline)
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
		if got := g.expired(w.at); got != w.expired {
			t.Errorf("wall clock %v: expired %v, want %v", w.at, got, w.expired)
		}
	}
	if g.expired(trustedNow()) {
		t.Fatal("expired an hour before its deadline")
	}
	if !g.expired(trustedNow().Add(time.Hour + time.Second)) {
		t.Fatal("not expired a second after its deadline on both clocks")
	}

	// The trusted time is the later of the wall clock and the monotonic
	// clock's estimate of it, measured from the process start.
	start := processStart.Round(0)
	// The wall clock was set back an hour, but by the monotonic clock the
	// deadline has passed.
	monotonicDeadline := start.Add(2 * time.Hour)
	setBack := &Generation{NotAfter: monotonicDeadline}
	if now := trustedTime(monotonicDeadline.Add(-time.Hour), 2*time.Hour); !setBack.expired(now) || !now.Equal(monotonicDeadline) {
		t.Fatalf("a wall clock set back extended the deadline (trusted time %v)", now)
	}
	// The monotonic clock paused (system suspend) while the wall clock moved
	// past the deadline.
	wallDeadline := start.Add(3 * time.Hour)
	suspended := &Generation{NotAfter: wallDeadline}
	if now := trustedTime(wallDeadline.Add(time.Millisecond), time.Hour); !suspended.expired(now) || !now.Equal(wallDeadline.Add(time.Millisecond)) {
		t.Fatalf("a paused monotonic clock extended the deadline (trusted time %v)", now)
	}
	// Neither clock has reached the deadline.
	if now := trustedTime(wallDeadline.Add(-time.Minute), time.Hour); suspended.expired(now) {
		t.Fatalf("expired before either clock reached the deadline (trusted time %v)", now)
	}
	confirmed, err := Compile(threeAPIPolicy(), 2, time.Time{})
	if err != nil || confirmed.expired(trustedNow().Add(1000*time.Hour)) {
		t.Fatalf("a confirmed generation expired: %v", err)
	}
}

// With no request at all, the controller rolls back an expired
// provisional base on its own. A timer that fires after a confirmation or an
// explicit rollback changes nothing.
func TestTimeoutWithoutTraffic(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(alice, bob), seedOptions{})
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
	if outcome := fromJSON[Outcome](t, body); code != http.StatusConflict || outcome.Reason != "ALREADY_RESOLVED" || outcome.Disposition != dispositionTimedOut {
		t.Fatalf("confirmation after the timeout: %d %s", code, body)
	}

	// Confirmed before its deadline: the timer must not undo it.
	_, kept := e.provisionalFrom(harness, e.cpaCandidate(), 1, 2, "quiet-timeout-02")
	if code, _, body := e.resolve(harness, "confirm", kept.Provisional, `"r1"`, "quiet-timeout-02-confirm"); code != http.StatusOK {
		t.Fatalf("confirmation: %d %s", code, body)
	}
	// Rolled back before its deadline: the timer must not act again.
	_, withdrawn := e.provisionalFrom(harness, e.protectedPolicy(alice, bob), kept.Provisional, 2, "quiet-timeout-03")
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
	want := []string{dispositionTimedOut, dispositionConfirmed, dispositionRolledBack}
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
	seed(t, e.database, e.protectedPolicy(alice, bob), seedOptions{})
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

// Without its timer (fault build), an expired provisional base still
// admits nothing: a confirmation at the decision instant is refused and
// rolls it back, and under traffic the first refused request makes the
// controller roll back, once, however many requests see the expiry at once.
func TestExpiredProvisionalAdmitsNothing(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(alice, bob), seedOptions{})
	e.start(binaries.fault, "GATEWAY_FAULT=no-deadline-timer")
	harness := e.mustLogin(harnessAdmin, harnessPassword)

	// A late confirmation, with nothing else having noticed the deadline.
	_, late := e.provisionalFrom(harness, e.cpaCandidate(), 1, 2, "late-confirmation")
	time.Sleep(time.Until(late.Deadline.Add(100 * time.Millisecond)))
	if s := e.status(); s.Provisional != late.Provisional {
		t.Fatalf("something other than the confirmation ended the provisional base: %+v", s)
	}
	code, outcome, body := e.resolve(harness, "confirm", late.Provisional, `"r1"`, "late-confirmation-confirm")
	if code != http.StatusConflict || outcome.Reason != "DEADLINE_PASSED" {
		t.Fatalf("late confirmation: %d %s", code, body)
	}
	if s := e.status(); s.Confirmed != 1 || s.Provisional != 0 || s.Revision != 1 {
		t.Fatalf("status after the late confirmation: %+v", s)
	}
	e.expectEnforced(false, "")

	// Traffic across the deadline. bob may write /a/item only under the
	// candidate; no write sent at or after the deadline may be admitted.
	_, expiring := e.provisionalFrom(harness, e.cpaCandidate(), 1, 2, "late-timer-traffic")
	bobToken := "Bearer " + issuer.tokenFor(t, "bob")
	e.resetReceivers()
	var admittedBefore, refusedAfter, baseAfter int
	for time.Now().Before(expiring.Deadline.Add(1500 * time.Millisecond)) {
		sent := time.Now()
		code, _ := e.request(http.MethodPost, "/a/item", "application/json", `{}`, "Authorization", bobToken)
		atOrAfter := !sent.Before(expiring.Deadline)
		switch {
		case code == http.StatusOK && atOrAfter:
			t.Fatalf("write sent %v after the deadline was admitted", sent.Sub(expiring.Deadline))
		case code == http.StatusOK:
			admittedBefore++
		case code == http.StatusServiceUnavailable && atOrAfter:
			refusedAfter++
		case code == http.StatusServiceUnavailable && expiring.Deadline.Sub(sent) < 200*time.Millisecond:
			// Sent just before the deadline but captured by the gateway at or
			// after it: refusing it is correct.
		case code == http.StatusForbidden && atOrAfter:
			baseAfter++
		default:
			t.Fatalf("write sent at %v relative to the deadline: %d", sent.Sub(expiring.Deadline), code)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if admittedBefore == 0 || refusedAfter == 0 || baseAfter == 0 {
		t.Fatalf("admitted before %d, refused after %d, base policy after %d", admittedBefore, refusedAfter, baseAfter)
	}
	for _, r := range e.c.receipts() {
		if r.Generation != expiring.Generation {
			t.Fatalf("C received a request of generation %s", r.Generation)
		}
	}
	if n := e.c.count(); n != admittedBefore {
		t.Fatalf("C received %d writes, %d were admitted", n, admittedBefore)
	}

	// Many requests see the expiry at once; one rollback happens.
	_, crowded := e.provisionalFrom(harness, e.cpaCandidate(), 1, 2, "late-timer-crowded")
	time.Sleep(time.Until(crowded.Deadline.Add(50 * time.Millisecond)))
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 20 {
		wg.Go(func() {
			<-start
			e.get("/b/x", "Authorization", bobToken)
		})
	}
	close(start)
	wg.Wait()
	waitFor(t, time.Now().Add(5*time.Second), "the rollback after concurrent expiry", func() bool { return e.status().Provisional == 0 })
	e.expectEnforced(false, "")

	e.stop()
	list := storedActivations(t, e.database)
	if len(list) != 3 {
		t.Fatalf("stored activations %+v", list)
	}
	for _, a := range list {
		if a.Disposition != dispositionTimedOut || a.Resolution != fmt.Sprintf("auto-rollback:%d", a.Revision) || a.ResolvedAt.Before(a.Deadline) {
			t.Fatalf("activation %+v", a)
		}
	}
	if n := queryInt(t, e.database, `SELECT COUNT(*) FROM transition WHERE id LIKE 'auto-rollback:%' AND outcome = 'committed'`); n != 3 {
		t.Fatalf("%d automatic rollbacks, want 3", n)
	}
	if n := queryInt(t, e.database, `SELECT COUNT(*) FROM transition WHERE kind = 'cpa.confirm' AND outcome = 'committed'`); n != 0 {
		t.Fatalf("%d confirmations committed", n)
	}
}

// Confirmations sent just before, at and just after the deadline
// are decided by the controller's clock at the moment it decides: a committed
// confirmation was decided before the deadline, and anything decided at or
// after it times out instead. The timer is disabled so the decision instant
// is the only arbiter.
func TestDecisionAtDeadline(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(alice, bob), seedOptions{})
	e.start(binaries.fault, "GATEWAY_FAULT=no-deadline-timer")
	harness := e.mustLogin(harnessAdmin, harnessPassword)

	confirmed := int64(1)
	answers := map[int64]int{}
	for i, offset := range []time.Duration{-40 * time.Millisecond, -10 * time.Millisecond, 0, 10 * time.Millisecond, 40 * time.Millisecond} {
		_, activated := e.provisionalFrom(harness, e.cpaCandidate(), confirmed, 1, fmt.Sprintf("boundary-round-%02d", i))
		time.Sleep(time.Until(activated.Deadline.Add(offset)))
		sent := time.Now()
		code, outcome, body := e.resolve(harness, "confirm", activated.Provisional, policyETag(confirmed), fmt.Sprintf("boundary-confirm-%02d", i))
		switch {
		case code == http.StatusOK && outcome.Disposition == dispositionConfirmed:
			if !sent.Before(activated.Deadline) {
				t.Fatalf("confirmation sent %v after the deadline was committed", sent.Sub(activated.Deadline))
			}
			confirmed = activated.Provisional
		case code == http.StatusConflict && outcome.Reason == "DEADLINE_PASSED":
		default:
			t.Fatalf("offset %v: %d %s", offset, code, body)
		}
		answers[activated.Provisional] = code
		t.Logf("confirmation sent %v from the deadline: %d %s", sent.Sub(activated.Deadline), code, outcome.Reason+outcome.Disposition)
	}

	e.stop()
	for _, a := range storedActivations(t, e.database) {
		switch a.Disposition {
		case dispositionConfirmed:
			if !a.ResolvedAt.Before(a.Deadline) || answers[a.Revision] != http.StatusOK {
				t.Fatalf("confirmed activation %+v decided at or after its deadline (answer %d)", a, answers[a.Revision])
			}
		case dispositionTimedOut:
			if a.ResolvedAt.Before(a.Deadline) || answers[a.Revision] != http.StatusConflict {
				t.Fatalf("timed-out activation %+v decided before its deadline (answer %d)", a, answers[a.Revision])
			}
		default:
			t.Fatalf("activation %+v", a)
		}
	}
}

// A restart before the deadline restores the provisional base with
// the same deadline; a gateway stopped across the deadline rolls back before
// it admits its first request.
func TestRestartAroundDeadline(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(alice, bob), seedOptions{})
	e.start(binaries.plain)
	harness := e.mustLogin(harnessAdmin, harnessPassword)

	_, kept := e.provisionalFrom(harness, e.cpaCandidate(), 1, 60, "restart-before-deadline")
	e.stop()
	e.start(binaries.plain)
	harness = e.mustLogin(harnessAdmin, harnessPassword)
	s := e.status()
	if s.Provisional != kept.Provisional || !s.Deadline.Equal(kept.Deadline) || !s.NotAfter.Equal(kept.Deadline) {
		t.Fatalf("status after restart %+v, activation %+v", s, kept)
	}
	e.expectEnforced(true, s.Generation)
	if code, _, body := e.resolve(harness, "rollback", kept.Provisional, `"r1"`, "restart-before-rollback"); code != http.StatusOK {
		t.Fatalf("rollback: %d %s", code, body)
	}

	_, expiring := e.provisionalFrom(harness, e.cpaCandidate(), 1, 2, "restart-after-deadline")
	e.stop()
	time.Sleep(time.Until(expiring.Deadline.Add(500 * time.Millisecond)))
	e.start(binaries.plain)
	// The very first request after the restart is decided by the base.
	e.resetReceivers()
	code, _ := e.request(http.MethodPost, "/a/item", "application/json", `{}`, "Authorization", "Bearer "+issuer.tokenFor(t, "bob"))
	if code != http.StatusForbidden {
		t.Fatalf("first request after restarting past the deadline: %d, want 403", code)
	}
	e.assertReceipts(0, 0, 0)
	if s := e.status(); s.Revision != 1 || s.Provisional != 0 || s.Confirmed != 1 {
		t.Fatalf("status %+v", s)
	}
	e.expectEnforced(false, "")
	e.stop()
	list := storedActivations(t, e.database)
	if len(list) != 2 || list[1].Disposition != dispositionTimedOut || list[1].ResolvedBy != "controller" || list[1].ResolvedAt.Before(list[1].Deadline) {
		t.Fatalf("stored activations %+v", list)
	}
}
