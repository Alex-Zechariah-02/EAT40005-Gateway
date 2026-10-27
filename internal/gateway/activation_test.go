package gateway

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// These tests drive the confirmed-activation lifecycle through the real
// Admin API of a running gateway. Every enforcement claim is checked at the
// receivers: a status code alone does not show what was forwarded.

// activate asks for a staged candidate to become the provisional base.
func (e *env) activate(s adminSession, c candidateRecord, seconds int64, ifMatch, key string) (int, Outcome, string) {
	e.t.Helper()
	body := string(mustJSON(map[string]any{"candidate": c.ID, "checksum": c.Checksum, "confirm_within_seconds": seconds}))
	code, data, _ := e.as(s, http.MethodPost, "/admin/provisional", body, "If-Match", ifMatch, "Idempotency-Key", key)
	var outcome Outcome
	json.Unmarshal([]byte(data), &outcome)
	return code, outcome, data
}

// resolve confirms ("confirm") or rolls back ("rollback") a provisional
// revision.
func (e *env) resolve(s adminSession, action string, revision int64, ifMatch, key string) (int, Outcome, string) {
	e.t.Helper()
	body := string(mustJSON(map[string]any{"revision": revision}))
	code, data, _ := e.as(s, http.MethodPost, "/admin/provisional/"+action, body, "If-Match", ifMatch, "Idempotency-Key", key)
	var outcome Outcome
	json.Unmarshal([]byte(data), &outcome)
	return code, outcome, data
}

// provisionalFrom stages policy against the confirmed revision parent and
// activates it, failing the test unless both succeed.
func (e *env) provisionalFrom(s adminSession, policy Policy, parent, seconds int64, key string) (candidateRecord, Outcome) {
	e.t.Helper()
	code, staged, body := e.stage(s, policy, policyETag(parent), "stage-"+key)
	if code != http.StatusCreated {
		e.t.Fatalf("staging: %d %s", code, body)
	}
	code, outcome, body := e.activate(s, staged.Candidate.candidateRecord, seconds, policyETag(parent), "activate-"+key)
	if code != http.StatusOK {
		e.t.Fatalf("activation: %d %s", code, body)
	}
	return staged.Candidate.candidateRecord, outcome
}

func (e *env) confirmPolicy(s adminSession, policy Policy, parent int64, key string) Outcome {
	e.t.Helper()
	_, activation := e.provisionalFrom(s, policy, parent, 60, key)
	code, outcome, body := e.resolve(s, "confirm", activation.Provisional, policyETag(parent), "confirm-"+key)
	if code != http.StatusOK {
		e.t.Fatalf("confirmation: %d %s", code, body)
	}
	return outcome
}

// storedActivations reads every activation record of a stopped gateway.
func storedActivations(t *testing.T, path string) []activationRecord {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT ` + activationColumns + ` FROM activation ORDER BY revision`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var list []activationRecord
	for rows.Next() {
		record, err := scanActivation(rows)
		if err != nil {
			t.Fatal(err)
		}
		list = append(list, record)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return list
}

// cpaCandidate differs from protectedPolicy(alice, bob) in three ways: bob
// (READER) loses GET /b/* and gains POST /a/item, and API a moves from
// upstream A to upstream C.
func (e *env) cpaCandidate() Policy {
	p := e.protectedPolicy()
	p.Upstreams = append(p.Upstreams, Upstream{ID: "C", Dial: e.c.addr})
	p.APIs[0].Upstream = "C"
	p.Permits = slices.DeleteFunc(p.Permits, func(permit Permit) bool { return permit == Permit{Role: "READER", Operation: "read-b"} })
	p.Permits = append(p.Permits, Permit{Role: "READER", Operation: "write-a"})
	return p
}

// expectEnforced sends three probes and checks the policy actually enforced:
// bob reading /b, bob writing /a and alice reading /a. Under the confirmed
// base bob may read /b (B) but not write, and /a is served by A; under the
// candidate bob may write but not read /b, and /a is served by C. A refused
// probe must reach no receiver. A non-empty generation must label every
// forwarded request.
func (e *env) expectEnforced(candidate bool, generation string) {
	e.t.Helper()
	e.resetReceivers()
	bobToken := "Bearer " + issuer.tokenFor(e.t, "bob")
	readB, _ := e.get("/b/x", "Authorization", bobToken)
	writeA, _ := e.request(http.MethodPost, "/a/item", "application/json", `{"n":1}`, "Authorization", bobToken)
	readA, _ := e.get("/a/x", "Authorization", "Bearer "+issuer.tokenFor(e.t, "alice"))

	want := [3]int{http.StatusOK, http.StatusForbidden, http.StatusOK}
	if candidate {
		want = [3]int{http.StatusForbidden, http.StatusOK, http.StatusOK}
	}
	if got := [3]int{readB, writeA, readA}; got != want {
		e.t.Fatalf("probes (bob GET /b, bob POST /a/item, alice GET /a) = %v, want %v (candidate policy: %v)", got, want, candidate)
	}
	if candidate {
		e.assertReceipts(0, 0, 2)
	} else {
		e.assertReceipts(1, 1, 0)
	}
	for _, rc := range []*receiver{e.a, e.b, e.c} {
		for _, r := range rc.receipts() {
			if generation != "" && r.Generation != generation {
				e.t.Fatalf("%s received a request of generation %s, want %s", rc.name, r.Generation, generation)
			}
		}
	}
}

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
	candidate := staged.Candidate.candidateRecord
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
		Activation activationRecord `json:"activation"`
		Generation string           `json:"generation"`
		Changes    []controlChange  `json:"changes"`
	}](t, body)
	wantChanges := []controlChange{{"api", "a"}, {"permit", "READER:read-b"}, {"permit", "READER:write-a"}, {"upstream", "C"}}
	if code != http.StatusOK || header.Get("ETag") != `"r1"` || view.Activation.Revision != 2 || view.Activation.RollbackTarget != 1 ||
		view.Activation.Disposition != "" || view.Generation != activated.Generation || !slices.Equal(view.Changes, wantChanges) {
		t.Fatalf("provisional view: %d %s", code, body)
	}

	// Ordinary traffic, reads, a retried activation, a refused second
	// activation and refused membership edits do not confirm or extend it.
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
	if code, _, body := e.activate(harness, other.Candidate.candidateRecord, 60, `"r1"`, "activate-confirm-path-2"); code != http.StatusConflict ||
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
	if code != http.StatusOK || confirmed.Outcome != "committed" || confirmed.Disposition != dispositionConfirmed ||
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
		again.Transition != confirmed.Transition || again.Disposition != dispositionConfirmed || again.Revision != 2 {
		t.Fatalf("retried confirmation: %d %s", code, body)
	}
	for _, action := range []string{"confirm", "rollback"} {
		code, outcome, body := e.resolve(harness, action, 2, `"r2"`, "after-confirm-"+action+"-x")
		if code != http.StatusConflict || outcome.Reason != "ALREADY_RESOLVED" || outcome.Disposition != dispositionConfirmed {
			t.Errorf("%s after confirmation: %d %s", action, code, body)
		}
	}
	if s := e.status(); s.Generation != confirmed.Generation || s.Confirmed != 2 {
		t.Fatalf("status after refused repeats: %+v", s)
	}

	// The audit trail has one intent and one outcome per transition, with the
	// lifecycle fields and no credential.
	events := e.eventsWhen(func(events []map[string]any) bool { return countKind(events, "cpa.confirmed") > 0 })
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
		!list[0].Deadline.Equal(activated.Deadline) || list[0].Disposition != dispositionConfirmed ||
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

// An explicit rollback restores the earlier confirmed base in real
// enforcement: the permission the candidate added stops working, the one it
// removed works again and the moved API returns to its original upstream.
// Rate counters carry over, so neither activation nor rollback grants a
// fresh quota.
func TestRollbackRestoresConfirmedBase(t *testing.T) {
	e := newEnv(t)
	base := e.protectedPolicy()
	base.Operations[0].Rate = rate(3, 60) // read-a: three per minute per subject
	seed(t, e.database, base, seedOptions{})
	e.start(binaries.plain)
	harness := e.mustLogin(harnessAdmin, harnessPassword)
	baseGeneration := e.status().Generation
	aliceToken := "Bearer " + issuer.tokenFor(t, "alice")

	// alice uses two of her three reads under the confirmed base.
	for range 2 {
		if code, _ := e.get("/a/x", "Authorization", aliceToken); code != http.StatusOK {
			t.Fatalf("read under the confirmed base: %d", code)
		}
	}

	candidate := e.cpaCandidate()
	candidate.Operations[0].Rate = rate(5, 60) // the candidate allows five
	_, activated := e.provisionalFrom(harness, candidate, 1, 60, "rollback-path-01")
	// Under the provisional rule two of five are used: three more.
	for i := range 4 {
		code, _ := e.get("/a/x", "Authorization", aliceToken)
		if want := http.StatusOK; i == 3 {
			want = http.StatusTooManyRequests
			if code != want {
				t.Fatalf("fourth provisional read: %d, want 429", code)
			}
		} else if code != want {
			t.Fatalf("provisional read %d: %d", i, code)
		}
	}
	if n := e.c.count(); n != 3 {
		t.Fatalf("C received %d reads, want 3", n)
	}

	code, rolledBack, body := e.resolve(harness, "rollback", 2, `"r1"`, "rollback-path-rollback")
	if code != http.StatusOK || rolledBack.Disposition != dispositionRolledBack || rolledBack.Revision != 1 ||
		rolledBack.Provisional != 2 || rolledBack.RollbackTarget != 1 || rolledBack.Generation == "" || rolledBack.Generation == activated.Generation {
		t.Fatalf("rollback: %d %s", code, body)
	}
	if s := e.status(); s.Revision != 1 || s.Confirmed != 1 || s.Provisional != 0 || !s.NotAfter.IsZero() || s.Generation != rolledBack.Generation {
		t.Fatalf("status after rollback: %+v", s)
	}
	// The restored rule (three) applies to the five reads already counted: no
	// free quota after the rollback.
	e.resetReceivers()
	if code, _ := e.get("/a/x", "Authorization", aliceToken); code != http.StatusTooManyRequests {
		t.Fatalf("read after rollback: %d, want 429", code)
	}
	e.assertReceipts(0, 0, 0)

	// bob's permissions and the destination of /a are the confirmed base's.
	e.resetReceivers()
	bobToken := "Bearer " + issuer.tokenFor(t, "bob")
	if code, _ := e.get("/b/x", "Authorization", bobToken); code != http.StatusOK {
		t.Fatalf("restored permission: %d", code)
	}
	if code, _ := e.request(http.MethodPost, "/a/item", "application/json", `{}`, "Authorization", bobToken); code != http.StatusForbidden {
		t.Fatalf("withdrawn permission: %d", code)
	}
	if code, _ := e.get("/a/x", "Authorization", bobToken); code != http.StatusOK {
		t.Fatalf("bob read of /a: %d", code)
	}
	e.assertReceipts(1, 1, 0)
	for _, r := range append(e.a.receipts(), e.b.receipts()...) {
		if r.Generation != rolledBack.Generation {
			t.Fatalf("request served by generation %s, want the restored %s", r.Generation, rolledBack.Generation)
		}
	}
	// A generation's identity is derived from its revision and content, so
	// the restored base is exactly the earlier confirmed one.
	if rolledBack.Generation != baseGeneration {
		t.Fatalf("restored generation %s, original confirmed generation %s", rolledBack.Generation, baseGeneration)
	}

	// Repeats: the same request replays, another is told the disposition, and
	// a confirmation of the rolled-back revision is refused.
	if code, again, body := e.resolve(harness, "rollback", 2, `"r1"`, "rollback-path-rollback"); code != http.StatusOK ||
		again.Transition != rolledBack.Transition || again.Disposition != dispositionRolledBack {
		t.Fatalf("retried rollback: %d %s", code, body)
	}
	for _, action := range []string{"rollback", "confirm"} {
		code, outcome, body := e.resolve(harness, action, 2, `"r1"`, "after-rollback-"+action+"-x")
		if code != http.StatusConflict || outcome.Reason != "ALREADY_RESOLVED" || outcome.Disposition != dispositionRolledBack {
			t.Errorf("%s after rollback: %d %s", action, code, body)
		}
	}
	// With no provisional base there is nothing to resolve.
	if code, outcome, body := e.resolve(harness, "rollback", 1, `"r1"`, "no-provisional-rollback"); code != http.StatusConflict || outcome.Reason != "NO_PROVISIONAL" {
		t.Fatalf("rollback without a provisional base: %d %s", code, body)
	}

	events := e.eventsWhen(func(events []map[string]any) bool { return countKind(events, "cpa.rolled_back") > 0 })
	if ev := findTransition(events, "cpa.rolled_back", rolledBack.Transition); ev == nil || ev["revision"] != float64(1) ||
		ev["provisional"] != float64(2) || ev["generation"] != rolledBack.Generation {
		t.Fatalf("rollback event %v", ev)
	}
	e.stop()
	list := storedActivations(t, e.database)
	if len(list) != 1 || list[0].Disposition != dispositionRolledBack || list[0].Resolution != rolledBack.Transition {
		t.Fatalf("stored activations %+v", list)
	}
	if n := queryInt(t, e.database, `SELECT COUNT(*) FROM base WHERE confirmed = 1 AND provisional IS NULL`); n != 1 {
		t.Fatal("base is not confirmed revision 1")
	}
}

// Activation is refused before anything is committed when the
// request or the stored candidate is invalid, stale or not eligible; the
// refusal is recorded and enforcement is unchanged.
func TestActivationRefusals(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.plain)
	harness := e.mustLogin(harnessAdmin, harnessPassword)

	code, staged, body := e.stage(harness, e.cpaCandidate(), `"r1"`, "stage-refusals-good")
	if code != http.StatusCreated {
		t.Fatalf("staging: %d %s", code, body)
	}
	good := staged.Candidate.candidateRecord
	code, staged, body = e.stage(harness, e.cpaCandidate(), `"r1"`, "stage-refusals-withdrawn")
	if code != http.StatusCreated {
		t.Fatalf("staging: %d %s", code, body)
	}
	withdrawn := staged.Candidate.candidateRecord
	if code, _, _ := e.as(harness, http.MethodDelete, fmt.Sprintf("/admin/candidates/%d", withdrawn.ID), "", "Idempotency-Key", "withdraw-refusals-x"); code != http.StatusOK {
		t.Fatalf("withdrawal: %d", code)
	}

	// An incomplete policy cannot even be staged, so it can never be
	// activated: a body rule without its schema and an operation without its
	// authentication setting.
	incomplete := mustJSON(e.cpaCandidate())
	for i, document := range []string{
		strings.Replace(string(incomplete), `,"schema":{"type":"object","additionalProperties":true}`, ``, 1),
		strings.Replace(string(incomplete), `"auth":"required",`, ``, 1),
	} {
		if code, _, body := e.stage(harness, json.RawMessage(document), `"r1"`, fmt.Sprintf("stage-incomplete-%02d", i)); code != http.StatusBadRequest {
			t.Errorf("incomplete candidate %d: %d %s", i, code, body)
		}
	}

	// Stored candidates that bypassed staging, as a damaged or outdated row
	// would: each is fully recompiled at activation and refused.
	e.stop()
	crafted := map[string]Policy{}
	duplicateRoute := e.cpaCandidate()
	duplicateRoute.Operations = append(duplicateRoute.Operations, Operation{ID: "read-a-again", API: "b", Method: "GET", Target: "/a/*", Access: "allow", Auth: "none"})
	crafted["DUPLICATE_OPERATION"] = duplicateRoute
	unknownRole := e.cpaCandidate()
	unknownRole.Permits = append(unknownRole.Permits, Permit{Role: "ADMIN", Operation: "read-a"})
	crafted["UNKNOWN_REFERENCE"] = unknownRole
	external := e.cpaCandidate()
	external.Upstreams[2].Dial = "10.0.0.1:80"
	crafted["INVALID_UPSTREAM"] = external
	noAuthentication := e.cpaCandidate()
	noAuthentication.Profile = "routing-only"
	crafted["UNSUPPORTED_FEATURE"] = noAuthentication
	db := inspect(t, e.database)
	ids := map[string]int64{}
	for reason, p := range crafted {
		document := mustJSON(p)
		result, err := db.Exec(`INSERT INTO candidate (parent, document, checksum, deny_all, status, actor, created_at) VALUES (1, ?, ?, 0, 'validated', 'x', 0)`,
			[]byte(document), documentChecksum(document))
		if err != nil {
			t.Fatal(err)
		}
		ids[reason], _ = result.LastInsertId()
	}
	// A candidate whose stored document no longer matches its checksum: the
	// checksum is the good candidate's, the document another policy.
	result, err := db.Exec(`INSERT INTO candidate (parent, document, checksum, deny_all, status, actor, created_at) VALUES (1, ?, ?, 0, 'validated', 'x', 0)`,
		[]byte(mustJSON(e.protectedPolicy())), good.Checksum)
	if err != nil {
		t.Fatal(err)
	}
	corruptID, _ := result.LastInsertId()
	db.Close()
	e.start(binaries.plain)
	harness = e.mustLogin(harnessAdmin, harnessPassword)
	before := e.status()

	cases := []struct {
		name      string
		candidate candidateRecord
		seconds   int64
		ifMatch   string
		status    int
		reason    string
		field     string
	}{
		{"unknown candidate", candidateRecord{ID: 999, Checksum: good.Checksum}, 60, `"r1"`, http.StatusNotFound, "NOT_FOUND", ""},
		{"withdrawn candidate", withdrawn, 60, `"r1"`, http.StatusConflict, "CANDIDATE_UNAVAILABLE", ""},
		{"checksum of another document", candidateRecord{ID: good.ID, Checksum: strings.Repeat("0", 64)}, 60, `"r1"`, http.StatusPreconditionFailed, "PRECONDITION_FAILED", "checksum"},
		{"stored document altered", candidateRecord{ID: corruptID, Checksum: good.Checksum}, 60, `"r1"`, http.StatusConflict, "CANDIDATE_INTEGRITY", ""},
		{"stale precondition", good, 60, `"r0"`, http.StatusPreconditionFailed, "PRECONDITION_FAILED", ""},
		{"zero period", good, 0, `"r1"`, http.StatusBadRequest, "INVALID_DEADLINE", "confirm_within_seconds"},
		{"negative period", good, -5, `"r1"`, http.StatusBadRequest, "INVALID_DEADLINE", "confirm_within_seconds"},
		{"period above a day", good, 86401, `"r1"`, http.StatusBadRequest, "INVALID_DEADLINE", "confirm_within_seconds"},
		{"period past the duration range", good, 10_000_000_000, `"r1"`, http.StatusBadRequest, "INVALID_DEADLINE", "confirm_within_seconds"},
	}
	for reason, id := range ids {
		cases = append(cases, struct {
			name      string
			candidate candidateRecord
			seconds   int64
			ifMatch   string
			status    int
			reason    string
			field     string
		}{"stored " + reason, candidateRecord{ID: id, Checksum: documentChecksum(mustJSON(crafted[reason]))}, 60, `"r1"`, http.StatusBadRequest, reason, ""})
	}
	for i, tc := range cases {
		code, outcome, body := e.activate(harness, tc.candidate, tc.seconds, tc.ifMatch, fmt.Sprintf("refused-activation-%02d", i))
		if code != tc.status || outcome.Outcome != "rejected" || outcome.Reason != tc.reason || (tc.field != "" && outcome.Field != tc.field) {
			t.Errorf("%s: %d %s, want %d %s", tc.name, code, body, tc.status, tc.reason)
		}
	}
	// Malformed periods are malformed requests.
	for i, period := range []string{`1.5`, `"60"`, `1e30`, `null`} {
		body := fmt.Sprintf(`{"candidate":%d,"checksum":%q,"confirm_within_seconds":%s}`, good.ID, good.Checksum, period)
		code, data, _ := e.as(harness, http.MethodPost, "/admin/provisional", body, "If-Match", `"r1"`, "Idempotency-Key", fmt.Sprintf("malformed-period-%02d", i))
		if period == `null` {
			// null leaves the required integer at zero: refused as out of range.
			if code != http.StatusBadRequest || !strings.Contains(data, "INVALID_DEADLINE") {
				t.Errorf("period %s: %d %s", period, code, data)
			}
			continue
		}
		if code != http.StatusBadRequest || !strings.Contains(data, "bad_request") {
			t.Errorf("period %s: %d %s", period, code, data)
		}
	}
	// Precondition header and identity are required.
	body = string(mustJSON(map[string]any{"candidate": good.ID, "checksum": good.Checksum, "confirm_within_seconds": 60}))
	if code, _, _ := e.as(harness, http.MethodPost, "/admin/provisional", body, "Idempotency-Key", "no-if-match-activation"); code != http.StatusPreconditionRequired {
		t.Errorf("activation without If-Match: %d", code)
	}
	if code, _, _ := e.as(harness, http.MethodPost, "/admin/provisional", body, "If-Match", `"r1"`); code != http.StatusBadRequest {
		t.Errorf("activation without Idempotency-Key: %d", code)
	}

	// Nothing changed and nothing new was reached.
	if after := e.status(); after.Generation != before.Generation || after.Provisional != 0 {
		t.Fatalf("refusals changed the status: %+v", after)
	}
	e.expectEnforced(false, before.Generation)
	events := e.eventsWhen(func(events []map[string]any) bool { return countKind(events, "cpa.rejected") >= len(cases) })
	if n := countKind(events, "cpa.rejected"); n < len(cases) {
		t.Fatalf("%d refusal events, want at least %d", n, len(cases))
	}
	if countKind(events, "cpa.activated") != 0 {
		t.Fatal("a refused activation recorded an activation")
	}

	// A candidate prepared on an older confirmed revision is stale once the
	// confirmed base has moved on.
	e.confirmPolicy(harness, e.protectedPolicy(), 1, "refusals-new-base")
	if code, outcome, body := e.activate(harness, good, 60, `"r2"`, "stale-candidate-activation"); code != http.StatusConflict || outcome.Reason != "CANDIDATE_STALE" {
		t.Fatalf("stale candidate: %d %s", code, body)
	}
	e.stop()
	if list := storedActivations(t, e.database); len(list) != 1 || list[0].Disposition != dispositionConfirmed {
		t.Fatalf("refusals changed the one intentionally confirmed activation: %+v", list)
	}
	if n := queryInt(t, e.database, `SELECT COUNT(*) FROM transition WHERE kind = 'cpa.activate' AND outcome = 'rejected'`); n < len(cases)+1 {
		t.Fatalf("%d recorded activation refusals", n)
	}
}

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
	c := staged.Candidate.candidateRecord
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
			codes[0], outcomes[0], _ = e.activate(harness, first.Candidate.candidateRecord, 60, `"r1"`, "race-activation-one")
		},
		func() {
			codes[1], outcomes[1], _ = e.activate(harness, second.Candidate.candidateRecord, 60, `"r1"`, "race-activation-two")
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
			codes[0], outcomes[0], _ = e.activate(harness, third.Candidate.candidateRecord, 60, `"r1"`, "race-same-key-0001")
		},
		func() {
			codes[1], outcomes[1], _ = e.activate(harness, third.Candidate.candidateRecord, 60, `"r1"`, "race-same-key-0001")
		},
	)
	if codes != [2]int{http.StatusOK, http.StatusOK} || outcomes[0].Transition != outcomes[1].Transition ||
		outcomes[0].Provisional != outcomes[1].Provisional || !outcomes[0].Deadline.Equal(outcomes[1].Deadline) {
		t.Fatalf("same key twice: %v %+v", codes, outcomes)
	}
	if s := e.status(); !s.Deadline.Equal(outcomes[0].Deadline) {
		t.Fatalf("deadline %v, receipt %v", s.Deadline, outcomes[0].Deadline)
	}
	if code, _, body := e.activate(harness, first.Candidate.candidateRecord, 60, `"r1"`, "race-same-key-0001"); code != http.StatusUnprocessableEntity ||
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
		if outcomes[won].Disposition == dispositionConfirmed {
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
		if a.Disposition != dispositionConfirmed && a.Disposition != dispositionRolledBack {
			t.Fatalf("activation %+v has no single explicit disposition", a)
		}
		n := queryInt(t, e.database, `SELECT COUNT(*) FROM transition WHERE kind IN ('cpa.confirm', 'cpa.rollback', 'cpa.timeout')
			AND outcome = 'committed' AND result_candidate = ?`, a.Candidate)
		if n != 1 {
			t.Fatalf("activation of candidate %d has %d committed dispositions", a.Candidate, n)
		}
	}
}

// The lifecycle against the three fixture APIs, whose own receipts
// and state show what was executed. The candidate withdraws customers' right
// to create orders and grants staff the sales report. The test ends in the
// stable state for subsequent requests: one confirmed base, no provisional
// base, every activation resolved, fixtures back at their seeds.
func TestLifecycleWithFixtures(t *testing.T) {
	e, fixtures := newFixtureEnv(t)
	harness := e.mustLogin(harnessAdmin, harnessPassword)
	seedHash := map[string]string{}
	for _, f := range loadCatalogue(t).Fixtures {
		seedHash[f.Service] = f.SeedSHA256
	}
	candidate := fixturePolicy(fixtures)
	candidate.Permits = slices.DeleteFunc(candidate.Permits, func(p Permit) bool { return p == Permit{Role: "CUSTOMER", Operation: "orders.create"} })
	candidate.Permits = append(candidate.Permits, Permit{Role: "STAFF", Operation: "reports.sales"})

	// probe resets the fixtures, then has a customer create an order and a
	// member of staff read the sales report, and checks each fixture's
	// receipts and state.
	probe := func(underCandidate bool) {
		t.Helper()
		for _, f := range fixtures {
			f.reset()
		}
		order := `{"customer":"cust-001","item":"ITM-001","quantity":1}`
		create, _ := e.request(http.MethodPost, "/orders", "application/json", order, bearer(t, "cust-001")...)
		sales, _ := e.get("/reports/sales", bearer(t, "staff-01")...)
		counts := receiptCounts(fixtures)
		ordersChanged := fixtures["orders"].state().SHA256 != seedHash["orders"]
		if underCandidate {
			if create != http.StatusForbidden || counts["orders"] != 0 || ordersChanged || sales != http.StatusOK || counts["reports"] != 1 {
				t.Fatalf("candidate policy: create %d, sales %d, receipts %v, orders changed %v", create, sales, counts, ordersChanged)
			}
		} else if create != http.StatusCreated || counts["orders"] != 1 || !ordersChanged || sales != http.StatusForbidden || counts["reports"] != 0 {
			t.Fatalf("confirmed base: create %d, sales %d, receipts %v, orders changed %v", create, sales, counts, ordersChanged)
		}
		if counts["inventory"] != 0 {
			t.Fatalf("inventory received %d requests", counts["inventory"])
		}
	}

	probe(false)
	_, first := e.provisionalFrom(harness, candidate, 1, 300, "fixtures-first")
	probe(true)
	if code, _, body := e.resolve(harness, "rollback", first.Provisional, `"r1"`, "fixtures-first-rollback"); code != http.StatusOK {
		t.Fatalf("rollback: %d %s", code, body)
	}
	probe(false)
	_, second := e.provisionalFrom(harness, candidate, 1, 300, "fixtures-second")
	probe(true)
	if code, _, body := e.resolve(harness, "confirm", second.Provisional, `"r1"`, "fixtures-second-confirm"); code != http.StatusOK {
		t.Fatalf("confirmation: %d %s", code, body)
	}
	probe(true)

	s := e.status()
	if s.Confirmed != second.Provisional || s.Provisional != 0 || !s.NotAfter.IsZero() {
		t.Fatalf("stable state: %+v", s)
	}
	if code, _, _ := e.as(harness, http.MethodGet, "/admin/provisional", ""); code != http.StatusNotFound {
		t.Fatalf("provisional view in the stable state: %d", code)
	}
	code, body, _ := e.as(harness, http.MethodGet, "/admin/activations", "")
	items := fromJSON[struct {
		Items []activationRecord `json:"items"`
	}](t, body).Items
	if code != http.StatusOK || len(items) != 2 || items[0].Disposition != dispositionRolledBack || items[1].Disposition != dispositionConfirmed {
		t.Fatalf("activation history: %d %s", code, body)
	}
	for name, f := range fixtures {
		if state := f.reset(); state.SHA256 != seedHash[name] || len(f.receipts()) != 0 {
			t.Fatalf("reset of %s: %s", name, state.SHA256)
		}
	}
}

// Under continuous traffic, every request is decided and forwarded
// by one generation while activations, rollbacks and a confirmation are
// published: a receiver only ever sees generations of the policy that sends
// requests to it, and every answer is one that policy gives.
func TestLifecycleUnderLoad(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.plain)
	harness := e.mustLogin(harnessAdmin, harnessPassword)
	baseGenerations := map[string]bool{e.status().Generation: true}
	candidateGenerations := map[string]bool{}

	stop := make(chan struct{})
	var unexpected sync.Map
	var forwarded, refused atomic.Int64
	var wg sync.WaitGroup
	bobToken := "Bearer " + issuer.tokenFor(t, "bob")
	aliceToken := "Bearer " + issuer.tokenFor(t, "alice")
	requests := []struct {
		method, path, body, token string
	}{
		{http.MethodGet, "/b/x", "", bobToken},
		{http.MethodPost, "/a/item", `{"n":1}`, bobToken},
		{http.MethodGet, "/a/x", "", aliceToken},
	}
	for i := range 6 {
		r := requests[i%len(requests)]
		wg.Go(func() {
			client := &http.Client{Timeout: 10 * time.Second}
			for {
				select {
				case <-stop:
					return
				default:
				}
				req, _ := http.NewRequest(r.method, "http://"+e.dataAddr+r.path, strings.NewReader(r.body))
				req.Header.Set("Authorization", r.token)
				if r.body != "" {
					req.Header.Set("Content-Type", "application/json")
				}
				resp, err := client.Do(req)
				if err != nil {
					unexpected.Store(r.method+" "+r.path+": "+err.Error(), true)
					continue
				}
				resp.Body.Close()
				switch resp.StatusCode {
				case http.StatusOK:
					forwarded.Add(1)
				case http.StatusForbidden:
					refused.Add(1)
				default:
					unexpected.Store(fmt.Sprintf("%s %s: %d", r.method, r.path, resp.StatusCode), true)
				}
			}
		})
	}

	confirmed := int64(1)
	for round := range 4 {
		time.Sleep(150 * time.Millisecond)
		_, activated := e.provisionalFrom(harness, e.cpaCandidate(), confirmed, 60, fmt.Sprintf("load-round-%02d", round))
		candidateGenerations[activated.Generation] = true
		time.Sleep(150 * time.Millisecond)
		action := "rollback"
		if round == 3 {
			action = "confirm"
		}
		code, outcome, body := e.resolve(harness, action, activated.Provisional, `"r1"`, fmt.Sprintf("load-round-%02d-%s", round, action))
		if code != http.StatusOK {
			t.Fatalf("%s: %d %s", action, code, body)
		}
		if action == "confirm" {
			candidateGenerations[outcome.Generation] = true
		} else {
			baseGenerations[outcome.Generation] = true
		}
	}
	time.Sleep(150 * time.Millisecond)
	close(stop)
	wg.Wait()

	unexpected.Range(func(key, _ any) bool {
		t.Errorf("unexpected answer: %v", key)
		return true
	})
	// A and B are reached only under the confirmed base, C only under the
	// candidate; anything else would be a request decided by one generation
	// and forwarded by another.
	for _, check := range []struct {
		rc      *receiver
		allowed map[string]bool
	}{{e.a, baseGenerations}, {e.b, baseGenerations}, {e.c, candidateGenerations}} {
		for _, r := range check.rc.receipts() {
			if !check.allowed[r.Generation] {
				t.Fatalf("%s received a request of generation %s", check.rc.name, r.Generation)
			}
		}
	}
	if e.a.count() == 0 || e.b.count() == 0 || e.c.count() == 0 || refused.Load() == 0 {
		t.Fatalf("traffic did not exercise both policies: A %d B %d C %d refused %d", e.a.count(), e.b.count(), e.c.count(), refused.Load())
	}
	t.Logf("forwarded %d, refused %d; receipts A %d B %d C %d", forwarded.Load(), refused.Load(), e.a.count(), e.b.count(), e.c.count())
}
