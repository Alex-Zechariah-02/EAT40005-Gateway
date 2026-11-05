// Decisions at and around the provisional deadline.

package gateway

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/store"
)

// Without its timer (fault build), an expired provisional base still
// admits nothing: a confirmation at the decision instant is refused and
// rolls it back, and under traffic the first refused request makes the
// controller roll back, once, however many requests see the expiry at once.
func TestExpiredProvisionalAdmitsNothing(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
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
		received := time.Now()
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
			// after it: refusing it is correct and may trigger rollback before
			// the next send. Count this observed expiry refusal too.
			if received.Before(expiring.Deadline) {
				t.Fatalf("expiry refusal arrived before the deadline: %v", received.Sub(expiring.Deadline))
			}
			refusedAfter++
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
		if a.Disposition != store.DispositionTimedOut || a.Resolution != fmt.Sprintf("auto-rollback:%d", a.Revision) || a.ResolvedAt.Before(a.Deadline) {
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
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
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
		case code == http.StatusOK && outcome.Disposition == store.DispositionConfirmed:
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
		case store.DispositionConfirmed:
			if !a.ResolvedAt.Before(a.Deadline) || answers[a.Revision] != http.StatusOK {
				t.Fatalf("confirmed activation %+v decided at or after its deadline (answer %d)", a, answers[a.Revision])
			}
		case store.DispositionTimedOut:
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
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
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
	if len(list) != 2 || list[1].Disposition != store.DispositionTimedOut || list[1].ResolvedBy != "controller" || list[1].ResolvedAt.Before(list[1].Deadline) {
		t.Fatalf("stored activations %+v", list)
	}
}
