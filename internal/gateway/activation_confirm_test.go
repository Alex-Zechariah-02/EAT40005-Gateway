// Confirmed activation and confirmation end to end.

package gateway

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/compile"
	"github.com/Alex-Zechariah-02/apiward/internal/store"
)

// A staged candidate changes nothing. Its activation publishes it
// as the provisional active base with a durable rollback target and deadline;
// enforcement follows it at once. Traffic, reads and a restart before the
// deadline do not confirm it. An explicit, matching, timely confirmation
// makes it the confirmed base, once.
func TestActivationAndConfirmation(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.plain)
	harness := e.mustLogin(harnessAdmin, harnessPassword)
	base := e.status()
	e.expectEnforced(false, base.Generation)

	// Staging changes nothing enforced.
	code, staged, body := e.stage(harness, e.cpaCandidate(), `"r1"`, "stage-confirm-path-1")
	if code != http.StatusCreated {
		t.Fatalf("staging: %d %s", code, body)
	}
	candidate := staged.Candidate.CandidateRecord
	if s := e.status(); s.Generation != base.Generation || s.Provisional != 0 {
		t.Fatalf("staging changed the status: %+v", s)
	}
	e.expectEnforced(false, base.Generation)

	// Activation.
	before := time.Now()
	code, activated, body := e.activate(harness, candidate, 60, `"r1"`, "activate-confirm-path-1")
	after := time.Now()
	if code != http.StatusOK || activated.Outcome != "committed" || activated.Revision != 2 || activated.Provisional != 2 ||
		activated.RollbackTarget != 1 || activated.Candidate != candidate.ID || activated.Generation == "" || activated.Disposition != "" {
		t.Fatalf("activation: %d %s", code, body)
	}
	if activated.Deadline.Before(before.Add(60*time.Second).Add(-time.Millisecond)) || activated.Deadline.After(after.Add(60*time.Second)) {
		t.Fatalf("deadline %v is not 60 s after the activation (%v to %v)", activated.Deadline, before, after)
	}
	s := e.status()
	if s.Revision != 2 || s.Generation != activated.Generation || s.Confirmed != 1 || s.Provisional != 2 ||
		!s.Deadline.Equal(activated.Deadline) || !s.NotAfter.Equal(activated.Deadline) {
		t.Fatalf("status after activation: %+v", s)
	}
	e.expectEnforced(true, activated.Generation)

	// The provisional view names the activation and every changed target.
	code, body, header := e.as(harness, http.MethodGet, "/admin/provisional", "")
	view := fromJSON[struct {
		Activation store.ActivationRecord  `json:"activation"`
		Generation string                  `json:"generation"`
		Changes    []compile.ControlChange `json:"changes"`
	}](t, body)
	wantChanges := []compile.ControlChange{{Control: "api", Target: "a"}, {Control: "permit", Target: "READER:read-b"}, {Control: "permit", Target: "READER:write-a"}, {Control: "upstream", Target: "C"}}
	if code != http.StatusOK || header.Get("ETag") != `"r1"` || view.Activation.Revision != 2 || view.Activation.RollbackTarget != 1 ||
		view.Activation.Disposition != "" || view.Generation != activated.Generation || !slices.Equal(view.Changes, wantChanges) {
		t.Fatalf("provisional view: %d %s", code, body)
	}

	// Ordinary traffic, reads, a retried activation, a refused second
	// activation and an account block do not confirm or extend it.
	for range 10 {
		e.expectEnforced(true, activated.Generation)
	}
	for _, path := range []string{"/admin/status", "/admin/policy", "/admin/provisional", "/admin/candidates", "/admin/history", "/admin/activations"} {
		if code, _, _ := e.as(harness, http.MethodGet, path, ""); code != http.StatusOK {
			t.Fatalf("GET %s: %d", path, code)
		}
	}
	if code, again, body := e.activate(harness, candidate, 60, `"r1"`, "activate-confirm-path-1"); code != http.StatusOK ||
		again.Transition != activated.Transition || !again.Deadline.Equal(activated.Deadline) || again.Provisional != 2 {
		t.Fatalf("retried activation: %d %s", code, body)
	}
	code, other, body := e.stage(harness, e.protectedPolicy(), `"r1"`, "stage-confirm-path-2")
	if code != http.StatusCreated {
		t.Fatalf("staging a second candidate: %d %s", code, body)
	}
	if code, _, body := e.activate(harness, other.Candidate.CandidateRecord, 60, `"r1"`, "activate-confirm-path-2"); code != http.StatusConflict ||
		!strings.Contains(body, "PROVISIONAL_ACTIVE") {
		t.Fatalf("second activation: %d %s", code, body)
	}
	if code, _, _ := e.blockAccount(harness, "carol", "independent containment", "account-provisional"); code != http.StatusOK {
		t.Fatalf("containment while provisional: %d", code)
	}
	if s := e.status(); s.Provisional != 2 || s.Confirmed != 1 || !s.Deadline.Equal(activated.Deadline) {
		t.Fatalf("status after traffic and reads: %+v", s)
	}

	// A restart before the deadline restores the same provisional base with
	// the same deadline, not a fresh one.
	e.stop()
	e.start(binaries.plain)
	harness = e.mustLogin(harnessAdmin, harnessPassword)
	if s := e.status(); s.Revision != 2 || s.Provisional != 2 || !s.Deadline.Equal(activated.Deadline) {
		t.Fatalf("status after restart: %+v", s)
	}
	restarted := e.status().Generation
	e.expectEnforced(true, restarted)

	// Wrong revision, stale precondition and unknown revision are refused.
	refusals := []struct {
		revision int64
		ifMatch  string
		reason   string
		status   int
	}{
		{1, `"r1"`, "PROVISIONAL_MISMATCH", http.StatusConflict},
		{7, `"r1"`, "PROVISIONAL_MISMATCH", http.StatusConflict},
		{2, `"r2"`, "PRECONDITION_FAILED", http.StatusPreconditionFailed},
		{2, `"r0"`, "PRECONDITION_FAILED", http.StatusPreconditionFailed},
	}
	for i, r := range refusals {
		code, outcome, body := e.resolve(harness, "confirm", r.revision, r.ifMatch, fmt.Sprintf("bad-confirm-%02d-xxxx", i))
		if code != r.status || outcome.Reason != r.reason {
			t.Errorf("confirm %d with %s: %d %s, want %d %s", r.revision, r.ifMatch, code, body, r.status, r.reason)
		}
	}
	if s := e.status(); s.Provisional != 2 || s.Generation != restarted {
		t.Fatalf("refused confirmations changed the status: %+v", s)
	}

	// Explicit confirmation.
	code, confirmed, body := e.resolve(harness, "confirm", 2, `"r1"`, "confirm-path-confirm")
	if code != http.StatusOK || confirmed.Outcome != "committed" || confirmed.Disposition != store.DispositionConfirmed ||
		confirmed.Revision != 2 || confirmed.Provisional != 2 || confirmed.RollbackTarget != 1 || confirmed.Generation == "" {
		t.Fatalf("confirmation: %d %s", code, body)
	}
	s = e.status()
	if s.Revision != 2 || s.Confirmed != 2 || s.Provisional != 0 || !s.NotAfter.IsZero() || s.Generation != confirmed.Generation {
		t.Fatalf("status after confirmation: %+v", s)
	}
	e.expectEnforced(true, confirmed.Generation)
	if code, _, header := e.as(harness, http.MethodGet, "/admin/policy", ""); code != http.StatusOK || header.Get("ETag") != `"r2"` {
		t.Fatalf("confirmed policy: %d %s", code, header.Get("ETag"))
	}
	if code, _, _ := e.as(harness, http.MethodGet, "/admin/provisional", ""); code != http.StatusNotFound {
		t.Fatalf("provisional view after confirmation: %d", code)
	}

	// The same request returns the recorded receipt; any other request for
	// the resolved revision learns its disposition and changes nothing.
	if code, again, body := e.resolve(harness, "confirm", 2, `"r1"`, "confirm-path-confirm"); code != http.StatusOK ||
		again.Transition != confirmed.Transition || again.Disposition != store.DispositionConfirmed || again.Revision != 2 {
		t.Fatalf("retried confirmation: %d %s", code, body)
	}
	for _, action := range []string{"confirm", "rollback"} {
		code, outcome, body := e.resolve(harness, action, 2, `"r2"`, "after-confirm-"+action+"-x")
		if code != http.StatusConflict || outcome.Reason != "ALREADY_RESOLVED" || outcome.Disposition != store.DispositionConfirmed {
			t.Errorf("%s after confirmation: %d %s", action, code, body)
		}
	}
	if s := e.status(); s.Generation != confirmed.Generation || s.Confirmed != 2 {
		t.Fatalf("status after refused repeats: %+v", s)
	}

	// The audit trail has one intent and one outcome per transition, with the
	// lifecycle fields and no credential.
	events := e.eventsWhen(func(events []map[string]any) bool { return hasKinds(events, "cpa.activated", "cpa.confirmed") })
	activatedEvent := findTransition(events, "cpa.activated", activated.Transition)
	confirmedEvent := findTransition(events, "cpa.confirmed", confirmed.Transition)
	if activatedEvent == nil || confirmedEvent == nil || countKind(events, "cpa.activated") != 1 || countKind(events, "cpa.confirmed") != 1 {
		t.Fatalf("lifecycle events: activated %v confirmed %v", activatedEvent, confirmedEvent)
	}
	if activatedEvent["actor"] != harnessAdmin || activatedEvent["provisional"] != float64(2) || activatedEvent["rollback_target"] != float64(1) ||
		activatedEvent["generation"] != activated.Generation || activatedEvent["candidate"] != float64(candidate.ID) || activatedEvent["deadline"] == nil {
		t.Fatalf("activation event %v", activatedEvent)
	}
	if confirmedEvent["revision"] != float64(2) || confirmedEvent["generation"] != confirmed.Generation {
		t.Fatalf("confirmation event %v", confirmedEvent)
	}
	assertAbsent(t, e, harness.cookie, harness.csrf, harnessPassword)

	// Durable state: one activation, confirmed by the harness administrator
	// under the confirmation's identity; the candidate is consumed.
	e.stop()
	list := storedActivations(t, e.database)
	if len(list) != 1 || list[0].Revision != 2 || list[0].Candidate != candidate.ID || list[0].RollbackTarget != 1 ||
		!list[0].Deadline.Equal(activated.Deadline) || list[0].Disposition != store.DispositionConfirmed ||
		list[0].ResolvedBy != harnessAdmin || list[0].Resolution != confirmed.Transition || !list[0].ResolvedAt.Before(list[0].Deadline) {
		t.Fatalf("stored activations %+v", list)
	}
	if n := queryInt(t, e.database, `SELECT COUNT(*) FROM base WHERE confirmed = 2 AND provisional IS NULL AND deadline IS NULL`); n != 1 {
		t.Fatal("base is not confirmed revision 2")
	}
	if status := queryString(t, e.database, `SELECT status FROM candidate WHERE id = ?`, candidate.ID); status != "established" {
		t.Fatalf("candidate status %q", status)
	}
	if n := queryInt(t, e.database, `SELECT COUNT(*) FROM transition WHERE kind LIKE 'cpa.%' AND outcome = 'committed' AND published_at IS NOT NULL AND audited_at IS NOT NULL`); n != 2 {
		t.Fatalf("%d published and audited lifecycle transitions, want 2", n)
	}
	if n := queryInt(t, e.database, `SELECT COUNT(*) FROM transition WHERE id LIKE 'auto-rollback:%'`); n != 0 {
		t.Fatalf("%d automatic rollbacks recorded", n)
	}
}
