// Confirmed activation helpers and rollback to the confirmed base.

package gateway

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"slices"
	"testing"
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
	db, err := sql.Open("sqlite", databaseURI(path))
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
