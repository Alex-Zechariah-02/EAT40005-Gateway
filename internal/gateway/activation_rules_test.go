// Activation authorisation and single provisional rules.

package gateway

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/Alex-Zechariah-02/apiward/internal/store"
)

// Every lifecycle operation needs an authenticated administrator
// with manage_policy, the admin origin and the session's CSRF token; reads of
// the lifecycle need read or read_history. A client API token is not an
// administrator credential. Another administrator's idempotency key never
// returns someone else's receipt.
func TestLifecycleNeedsAuthorisedAdministrator(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.plain)
	// Each enrolment restarts the gateway, so everyone logs in afterwards.
	_, readerPassword := e.addAdmin(binaries.plain, "reader", "read")
	_, operatorPassword := e.addAdmin(binaries.plain, "operator", "read,read_history,manage_policy")
	harness := e.mustLogin(harnessAdmin, harnessPassword)
	reader := e.mustLogin("reader", readerPassword)
	operator := e.mustLogin("operator", operatorPassword)

	code, staged, body := e.stage(harness, e.cpaCandidate(), `"r1"`, "stage-authorisation")
	if code != http.StatusCreated {
		t.Fatalf("staging: %d %s", code, body)
	}
	c := staged.Candidate.CandidateRecord
	activation := string(mustJSON(map[string]any{"candidate": c.ID, "checksum": c.Checksum, "confirm_within_seconds": 60}))
	resolution := `{"revision":2}`
	apiToken := "Bearer " + issuer.tokenFor(t, "alice")
	before := e.status()

	type attempt struct {
		name   string
		header []string
		status int
	}
	attempts := []attempt{
		{"no session", []string{"Origin", e.adminOrigin()}, http.StatusUnauthorized},
		{"API client token", []string{"Origin", e.adminOrigin(), "Authorization", apiToken}, http.StatusUnauthorized},
		{"read-only administrator", reader.headers(e), http.StatusForbidden},
		{"no CSRF token", []string{"Cookie", sessionCookie + "=" + harness.cookie, "Origin", e.adminOrigin()}, http.StatusForbidden},
		{"another origin", []string{"Cookie", sessionCookie + "=" + harness.cookie, csrfHeader, harness.csrf, "Origin", "http://evil.example"}, http.StatusForbidden},
	}
	for _, path := range []string{"/admin/provisional", "/admin/provisional/confirm", "/admin/provisional/rollback"} {
		payload := activation
		if path != "/admin/provisional" {
			payload = resolution
		}
		for i, a := range attempts {
			header := append(slices.Clone(a.header), "If-Match", `"r1"`, "Idempotency-Key", fmt.Sprintf("authorisation-%02d-xxxx", i))
			if code, body, _ := e.adminSend(http.MethodPost, path, payload, header...); code != a.status {
				t.Errorf("POST %s, %s: %d %s, want %d", path, a.name, code, body, a.status)
			}
		}
	}
	if code, _, _ := e.adminSend(http.MethodGet, "/admin/provisional", ""); code != http.StatusUnauthorized {
		t.Errorf("anonymous provisional view: %d", code)
	}
	if code, _, _ := e.as(reader, http.MethodGet, "/admin/activations", ""); code != http.StatusForbidden {
		t.Errorf("activation history without read_history: %d", code)
	}
	if after := e.status(); after.Generation != before.Generation || after.Provisional != 0 {
		t.Fatalf("refused calls changed the status: %+v", after)
	}

	// The operator activates; the harness administrator's same key and body
	// is its own new request, refused because a provisional base exists.
	if code, outcome, body := e.activate(operator, c, 60, `"r1"`, "shared-key-0000001"); code != http.StatusOK || outcome.Provisional != 2 {
		t.Fatalf("operator activation: %d %s", code, body)
	}
	if code, outcome, body := e.activate(harness, c, 60, `"r1"`, "shared-key-0000001"); code != http.StatusConflict || outcome.Reason != "PROVISIONAL_ACTIVE" ||
		outcome.Transition != "admin:"+harnessAdmin+":shared-key-0000001" {
		t.Fatalf("same key, other administrator: %d %s", code, body)
	}
	// A reader can see the provisional base but not resolve it.
	if code, _, _ := e.as(reader, http.MethodGet, "/admin/provisional", ""); code != http.StatusOK {
		t.Fatalf("reader provisional view: %d", code)
	}
	if code, _, _ := e.resolve(reader, "rollback", 2, `"r1"`, "reader-rollback-xxxx"); code != http.StatusForbidden {
		t.Fatalf("reader rollback: %d", code)
	}
	if s := e.status(); s.Provisional != 2 {
		t.Fatalf("status %+v", s)
	}
	e.expectEnforced(true, "")
	events := e.eventsWhen(func(events []map[string]any) bool { return countKind(events, "admin.denied") > 0 })
	if countKind(events, "admin.denied") == 0 || countKind(events, "admin.rejected") == 0 {
		t.Fatal("refused administration calls were not recorded")
	}
	assertAbsent(t, e, apiToken[len("Bearer "):], harness.cookie, reader.cookie, harness.csrf)
}

// Concurrent requests never produce a second provisional base or
// two dispositions: competing activations, repeated activations under one
// key, confirmation against rollback and confirmation against confirmation
// each have exactly one winner, and every loser is told what happened.
func TestOneProvisionalAndOneDisposition(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.plain)
	harness := e.mustLogin(harnessAdmin, harnessPassword)

	// concurrently runs each call at the same moment from its own goroutine.
	concurrently := func(calls ...func()) {
		var wg sync.WaitGroup
		start := make(chan struct{})
		for _, call := range calls {
			wg.Go(func() {
				<-start
				call()
			})
		}
		close(start)
		wg.Wait()
	}

	// Two candidates race for activation.
	_, first, _ := e.stage(harness, e.cpaCandidate(), `"r1"`, "stage-race-first")
	_, second, _ := e.stage(harness, e.protectedPolicy(), `"r1"`, "stage-race-second")
	var codes [2]int
	var outcomes [2]Outcome
	concurrently(
		func() {
			codes[0], outcomes[0], _ = e.activate(harness, first.Candidate.CandidateRecord, 60, `"r1"`, "race-activation-one")
		},
		func() {
			codes[1], outcomes[1], _ = e.activate(harness, second.Candidate.CandidateRecord, 60, `"r1"`, "race-activation-two")
		},
	)
	slices.Sort(codes[:])
	if codes != [2]int{http.StatusOK, http.StatusConflict} {
		t.Fatalf("competing activations answered %v %+v", codes, outcomes)
	}
	winner := outcomes[0]
	if winner.Outcome != "committed" {
		winner = outcomes[1]
	}
	if loser := outcomes[0].Reason + outcomes[1].Reason; loser != "PROVISIONAL_ACTIVE" {
		t.Fatalf("losing activation reason %q", loser)
	}
	if s := e.status(); s.Provisional != winner.Provisional || s.Generation != winner.Generation {
		t.Fatalf("status %+v, winner %+v", s, winner)
	}
	if code, _, body := e.resolve(harness, "rollback", winner.Provisional, `"r1"`, "race-cleanup-rollback-1"); code != http.StatusOK {
		t.Fatalf("rollback: %d %s", code, body)
	}

	// One key sent twice at once applies once; both get the same receipt.
	_, third, _ := e.stage(harness, e.cpaCandidate(), `"r1"`, "stage-race-third")
	concurrently(
		func() {
			codes[0], outcomes[0], _ = e.activate(harness, third.Candidate.CandidateRecord, 60, `"r1"`, "race-same-key-0001")
		},
		func() {
			codes[1], outcomes[1], _ = e.activate(harness, third.Candidate.CandidateRecord, 60, `"r1"`, "race-same-key-0001")
		},
	)
	if codes != [2]int{http.StatusOK, http.StatusOK} || outcomes[0].Transition != outcomes[1].Transition ||
		outcomes[0].Provisional != outcomes[1].Provisional || !outcomes[0].Deadline.Equal(outcomes[1].Deadline) {
		t.Fatalf("same key twice: %v %+v", codes, outcomes)
	}
	if s := e.status(); !s.Deadline.Equal(outcomes[0].Deadline) {
		t.Fatalf("deadline %v, receipt %v", s.Deadline, outcomes[0].Deadline)
	}
	if code, _, body := e.activate(harness, first.Candidate.CandidateRecord, 60, `"r1"`, "race-same-key-0001"); code != http.StatusUnprocessableEntity ||
		!strings.Contains(body, "TRANSITION_ID_REUSED") {
		t.Fatalf("same key, other candidate: %d %s", code, body)
	}
	if code, _, body := e.resolve(harness, "rollback", outcomes[0].Provisional, `"r1"`, "race-cleanup-rollback-2"); code != http.StatusOK {
		t.Fatalf("rollback: %d %s", code, body)
	}

	// Confirmation against rollback, and confirmation against confirmation,
	// over several rounds. The confirmed base moves with each confirmation.
	confirmed, candidateConfirmed := int64(1), false
	for round := range 6 {
		next := e.cpaCandidate()
		if candidateConfirmed {
			next = e.protectedPolicy()
		}
		_, activated := e.provisionalFrom(harness, next, confirmed, 60, fmt.Sprintf("race-round-%02d", round))
		actions := [2]string{"confirm", "rollback"}
		if round%2 == 1 {
			actions = [2]string{"confirm", "confirm"}
		}
		ifMatch := policyETag(confirmed)
		concurrently(
			func() {
				codes[0], outcomes[0], _ = e.resolve(harness, actions[0], activated.Provisional, ifMatch, fmt.Sprintf("race-%02d-first-xxxx", round))
			},
			func() {
				codes[1], outcomes[1], _ = e.resolve(harness, actions[1], activated.Provisional, ifMatch, fmt.Sprintf("race-%02d-second-xxx", round))
			},
		)
		won := 0
		if codes[1] == http.StatusOK {
			won = 1
		}
		lost := 1 - won
		if codes[won] != http.StatusOK || codes[lost] != http.StatusConflict || outcomes[lost].Reason != "ALREADY_RESOLVED" ||
			outcomes[lost].Disposition != outcomes[won].Disposition {
			t.Fatalf("round %d (%v): %v %+v", round, actions, codes, outcomes)
		}
		if outcomes[won].Disposition == store.DispositionConfirmed {
			confirmed, candidateConfirmed = activated.Provisional, !candidateConfirmed
		}
		if s := e.status(); s.Confirmed != confirmed || s.Provisional != 0 || s.Generation != outcomes[won].Generation {
			t.Fatalf("round %d status %+v", round, s)
		}
		e.expectEnforced(candidateConfirmed, outcomes[won].Generation)
	}

	e.stop()
	list := storedActivations(t, e.database)
	if len(list) != 8 {
		t.Fatalf("%d activations stored, want 8", len(list))
	}
	for _, a := range list {
		if a.Disposition != store.DispositionConfirmed && a.Disposition != store.DispositionRolledBack {
			t.Fatalf("activation %+v has no single explicit disposition", a)
		}
		n := queryInt(t, e.database, `SELECT COUNT(*) FROM transition WHERE kind IN ('cpa.confirm', 'cpa.rollback', 'cpa.timeout')
			AND outcome = 'committed' AND result_candidate = ?`, a.Candidate)
		if n != 1 {
			t.Fatalf("activation of candidate %d has %d committed dispositions", a.Candidate, n)
		}
	}
}
