package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Alex-Zechariah-02/apiward/internal/policy"
	"github.com/Alex-Zechariah-02/apiward/internal/store"
)

// Failure to read metadata after a committed stage must retain its receipt.
// An identical retry never creates another candidate, even while reads fail.
func TestCandidateReceiptSurvivesMetadataReadFailure(t *testing.T) {
	e := newEnv(t)
	policy := e.protectedPolicy()
	seed(t, e.database, policy, seedOptions{})
	e.start(binaries.fault, "GATEWAY_FAULT=candidate-read-unavailable")
	admin := e.mustLogin(harnessAdmin, harnessPassword)
	before := e.status()
	body := string(mustJSON(map[string]any{"policy": policy}))
	var receipt Outcome
	for range 2 {
		code, answer, _ := e.as(admin, http.MethodPost, "/admin/candidates", body,
			"If-Match", `"r1"`, "Idempotency-Key", "candidate-metadata-failure")
		receipt = fromJSON[Outcome](t, answer)
		if code != http.StatusServiceUnavailable || receipt.Outcome != "committed" || receipt.Reason != "STORE_ERROR" ||
			receipt.Candidate != 1 || receipt.Transition != "admin:"+harnessAdmin+":candidate-metadata-failure" {
			t.Fatalf("committed stage with unreadable metadata: %d %s", code, answer)
		}
	}
	if after := e.status(); after.Generation != before.Generation || after.Confirmed != before.Confirmed {
		t.Fatal("staging changed enforced authority")
	}
	e.assertReceipts(0, 0, 0)
	e.stop()
	if queryInt(t, e.database, `SELECT count(*) FROM candidate`) != 1 ||
		queryInt(t, e.database, `SELECT count(*) FROM transition WHERE kind = 'candidate.create' AND outcome = 'committed' AND audited_at IS NOT NULL`) != 1 {
		t.Fatal("candidate or audited transition was lost or duplicated")
	}
	e.start(binaries.plain)
	admin = e.mustLogin(harnessAdmin, harnessPassword)
	code, replayed, answer := e.stage(admin, policy, `"r1"`, "candidate-metadata-failure")
	if code != http.StatusCreated || replayed.Receipt.Candidate != receipt.Candidate ||
		replayed.Receipt.Transition != receipt.Transition || replayed.Candidate.ID != receipt.Candidate || replayed.Candidate.Status != "validated" {
		t.Fatalf("retry after metadata reads recover: %d %s", code, answer)
	}
	if len(e.candidateList(admin)) != 1 {
		t.Fatal("retry created another candidate")
	}
}

// candidateList reads every stored candidate.
func (e *env) candidateList(s adminSession) []store.CandidateRecord {
	e.t.Helper()
	code, body, _ := e.as(s, http.MethodGet, "/admin/candidates?limit=100", "")
	if code != http.StatusOK {
		e.t.Fatalf("listing candidates: %d %s", code, body)
	}
	return fromJSON[struct {
		Items []store.CandidateRecord `json:"items"`
	}](e.t, body).Items
}

// A valid candidate is validated, stored immutably and
// inspectable, and changes nothing that is enforced, including a staged
// change of an API's destination.
func TestCandidateStagingChangesNothingEnforced(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.plain)
	harness := e.mustLogin(harnessAdmin, harnessPassword)
	before := e.status()
	aliceToken := issuer.tokenFor(t, "alice")

	// The candidate moves API a from upstream A to upstream B.
	moved := e.protectedPolicy()
	moved.APIs[0].Upstream = "B"
	code, staged, body := e.stage(harness, moved, `"r1"`, "stage-moved-api-1")
	if code != http.StatusCreated {
		t.Fatalf("staging: %d %s", code, body)
	}
	c := staged.Candidate
	if staged.Receipt.Outcome != "committed" || staged.Receipt.Candidate != c.ID || staged.Receipt.Revision != 0 || staged.Receipt.Generation != "" {
		t.Fatalf("receipt %+v", staged.Receipt)
	}
	if c.Active == nil || *c.Active || c.Status != "validated" || c.Parent != 1 || c.DenyAll || c.Actor != harnessAdmin {
		t.Fatalf("stored candidate %s", body)
	}

	// The stored document is the submitted policy, under its checksum.
	code, body, _ = e.as(harness, http.MethodGet, fmt.Sprintf("/admin/candidates/%d", c.ID), "")
	view := fromJSON[struct {
		store.CandidateRecord
		Active   bool            `json:"active"`
		Document json.RawMessage `json:"document"`
	}](t, body)
	if code != http.StatusOK || view.Active || string(view.Document) != string(mustJSON(moved)) || store.DocumentChecksum(view.Document) != c.Checksum {
		t.Fatalf("candidate read: %d %s", code, body)
	}

	// Nothing enforced changed: revision, generation, confirmed policy and
	// the destination real traffic reaches.
	if after := e.status(); after.Revision != before.Revision || after.Generation != before.Generation {
		t.Fatalf("staging changed the status: %+v -> %+v", before, after)
	}
	_, body, header := e.as(harness, http.MethodGet, "/admin/policy", "")
	confirmed, err := policy.DecodePolicy(fromJSON[store.RevisionInfo](t, body).Document)
	if err != nil || header.Get("ETag") != `"r1"` || confirmed.APIs[0].Upstream != "A" {
		t.Fatalf("confirmed policy after staging: %v %s", err, body)
	}
	if code, _ := e.get("/a/x", "Authorization", "Bearer "+aliceToken); code != http.StatusOK {
		t.Fatalf("request after staging: %d", code)
	}
	e.assertReceipts(1, 0, 0)
	if g := e.a.receipts()[0].Generation; g != before.Generation {
		t.Fatalf("request served under %s, want %s", g, before.Generation)
	}

	// The audit trail records a committed candidate and no publication.
	events := e.eventsWhen(func(events []map[string]any) bool {
		for _, ev := range events {
			if ev["kind"] == "transition.committed" && ev["transition"] == staged.Receipt.Transition {
				return true
			}
		}
		return false
	})
	for _, ev := range events {
		if ev["transition"] == staged.Receipt.Transition && ev["kind"] == "transition.published" {
			t.Fatalf("staging recorded a publication: %v", ev)
		}
	}
	e.stop()
	if n := queryInt(t, e.database, `SELECT COUNT(*) FROM revision`); n != 1 {
		t.Fatalf("%d revisions after staging, want 1", n)
	}
}

// Stale or missing preconditions and invalid
// candidates are refused, nothing is stored, and the refusal is recorded.
func TestCandidateRefusals(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.plain)
	harness := e.mustLogin(harnessAdmin, harnessPassword)
	before := e.status()

	duplicateID := e.protectedPolicy()
	duplicateID.Operations = append(duplicateID.Operations, policy.Operation{ID: "read-a", API: "a", Method: "GET", Target: "/z/*", Access: "deny", Auth: "none"})
	duplicateRoute := e.protectedPolicy()
	duplicateRoute.Operations = append(duplicateRoute.Operations, policy.Operation{ID: "read-a-again", API: "b", Method: "GET", Target: "/a/*", Access: "deny", Auth: "none"})
	badTarget := e.protectedPolicy()
	badTarget.Operations[0].Target = "/a/../admin/*"
	badMethod := e.protectedPolicy()
	badMethod.Operations[0].Method = "CONNECT"
	remoteName := e.protectedPolicy()
	remoteName.Upstreams[0].Dial = "example.com:80"
	remoteAddress := e.protectedPolicy()
	remoteAddress.Upstreams[0].Dial = "10.0.0.1:80"
	unknownUpstream := e.protectedPolicy()
	unknownUpstream.Upstreams = unknownUpstream.Upstreams[1:] // API a and d still name A
	roleStillHeld := e.protectedPolicy()
	roleStillHeld.Roles = []policy.Role{{ID: "READER"}} // alice still holds WRITER
	roleStillHeld.Permits = []policy.Permit{{Role: "WRITER", Operation: "read-a"}}
	futureSchema := mustJSON(e.protectedPolicy())
	futureSchema = []byte(strings.Replace(string(futureSchema), fmt.Sprintf(`"schema_version":%d`, policy.SchemaVersion), `"schema_version":99`, 1))

	cases := []struct {
		name   string
		policy any
		reason string
		field  string
	}{
		{"duplicate operation id", duplicateID, "DUPLICATE_ID", "operations[4].id"},
		{"duplicate method and route", duplicateRoute, "DUPLICATE_OPERATION", "operations[4]"},
		{"ambiguous target", badTarget, "INVALID_TARGET", "operations[0].target"},
		{"unsupported method", badMethod, "INVALID_METHOD", "operations[0].method"},
		{"host name destination", remoteName, "INVALID_UPSTREAM", "upstreams[0].dial"},
		{"non-loopback destination", remoteAddress, "INVALID_UPSTREAM", "upstreams[0].dial"},
		{"removed upstream still referenced", unknownUpstream, "UNKNOWN_REFERENCE", "apis[0].upstream"},
		{"removed role still referenced by permit", roleStillHeld, "UNKNOWN_REFERENCE", "permits[0].role"},
		{"unsupported schema version", json.RawMessage(futureSchema), "SCHEMA_VERSION_UNSUPPORTED", "schema_version"},
	}
	for i, tc := range cases {
		// A refusal answers with the outcome itself, not a receipt and candidate.
		code, _, body := e.stage(harness, tc.policy, `"r1"`, fmt.Sprintf("invalid-candidate-%02d", i))
		outcome := fromJSON[Outcome](t, body)
		if code != http.StatusBadRequest || outcome.Outcome != "rejected" || outcome.Reason != tc.reason || outcome.Field != tc.field {
			t.Errorf("%s: %d %s, want 400 %s %s", tc.name, code, body, tc.reason, tc.field)
		}
	}

	valid := e.protectedPolicy()
	code, body, _ := e.as(harness, http.MethodPost, "/admin/candidates", string(mustJSON(map[string]any{"policy": valid})), "Idempotency-Key", "no-precondition-01")
	if code != http.StatusPreconditionRequired {
		t.Errorf("no If-Match: %d %s", code, body)
	}
	code, body, _ = e.as(harness, http.MethodPost, "/admin/candidates", string(mustJSON(map[string]any{"policy": valid})), "If-Match", `"r1"`)
	if code != http.StatusBadRequest {
		t.Errorf("no Idempotency-Key: %d %s", code, body)
	}
	for _, stale := range []string{`"r0"`, `"r2"`} {
		code, _, body := e.stage(harness, valid, stale, "stale-candidate-"+strings.Trim(stale, `"`)+"-x")
		if code != http.StatusPreconditionFailed || !strings.Contains(body, "PRECONDITION_FAILED") {
			t.Errorf("If-Match %s: %d %s", stale, code, body)
		}
	}
	oversized := e.protectedPolicy()
	oversized.Upstreams[0].ID = strings.Repeat("u", 90<<10)
	if code, _, _ := e.stage(harness, oversized, `"r1"`, "oversized-candidate"); code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized candidate: %d", code)
	}

	if list := e.candidateList(harness); len(list) != 0 {
		t.Fatalf("refused candidates were stored: %+v", list)
	}
	if after := e.status(); after.Revision != before.Revision || after.Generation != before.Generation {
		t.Fatalf("refusals changed the status: %+v", after)
	}

	// A refusal is recorded under its identity and replayed, never re-run.
	id := "admin:" + harnessAdmin + ":invalid-candidate-04"
	code, body, _ = e.as(harness, http.MethodGet, "/admin/history/"+id, "")
	if entry := fromJSON[store.HistoryEntry](t, body); code != http.StatusOK || entry.Outcome != "rejected" || entry.Reason != "INVALID_UPSTREAM" || entry.Candidate != 0 {
		t.Fatalf("history of a refused candidate: %d %s", code, body)
	}
	if code, _, body := e.stage(harness, remoteName, `"r1"`, "invalid-candidate-04"); code != http.StatusBadRequest || !strings.Contains(body, "INVALID_UPSTREAM") {
		t.Fatalf("replayed refusal: %d %s", code, body)
	}
	if code, _, body := e.stage(harness, valid, `"r1"`, "invalid-candidate-04"); code != http.StatusUnprocessableEntity || !strings.Contains(body, "TRANSITION_ID_REUSED") {
		t.Fatalf("refused identity reused for a valid candidate: %d %s", code, body)
	}
	e.assertReceipts(0, 0, 0)
}

// A candidate is immutable; it can only be
// withdrawn, once, and a retried submission returns the same candidate.
func TestCandidateLifecycle(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.plain)
	harness := e.mustLogin(harnessAdmin, harnessPassword)

	policy := e.protectedPolicy()
	code, first, body := e.stage(harness, policy, `"r1"`, "lifecycle-candidate")
	if code != http.StatusCreated {
		t.Fatalf("staging: %d %s", code, body)
	}
	code, again, body := e.stage(harness, policy, `"r1"`, "lifecycle-candidate")
	if code != http.StatusCreated || again.Candidate.ID != first.Candidate.ID || again.Receipt.Transition != first.Receipt.Transition {
		t.Fatalf("retried submission: %d %s", code, body)
	}
	if code, _, body := e.stage(harness, withoutRole(e.protectedPolicy(), "WRITER"), `"r1"`, "lifecycle-candidate"); code != http.StatusUnprocessableEntity || !strings.Contains(body, "TRANSITION_ID_REUSED") {
		t.Fatalf("key reused for another body: %d %s", code, body)
	}
	if list := e.candidateList(harness); len(list) != 1 {
		t.Fatalf("%d candidates stored, want 1", len(list))
	}

	path := fmt.Sprintf("/admin/candidates/%d", first.Candidate.ID)
	for _, method := range []string{http.MethodPut, http.MethodPatch, http.MethodPost} {
		if code, _, _ := e.as(harness, method, path, string(mustJSON(map[string]any{"policy": e.protectedPolicy()}))); code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: %d", method, path, code)
		}
	}

	code, body, _ = e.as(harness, http.MethodDelete, path, "", "Idempotency-Key", "lifecycle-withdraw")
	withdrawn := fromJSON[Outcome](t, body)
	if code != http.StatusOK || withdrawn.Outcome != "committed" || withdrawn.Candidate != first.Candidate.ID {
		t.Fatalf("withdrawal: %d %s", code, body)
	}
	if code, body, _ := e.as(harness, http.MethodDelete, path, "", "Idempotency-Key", "lifecycle-withdraw"); code != http.StatusOK || fromJSON[Outcome](t, body) != withdrawn {
		t.Fatalf("retried withdrawal: %d %s", code, body)
	}
	if code, body, _ := e.as(harness, http.MethodDelete, path, "", "Idempotency-Key", "lifecycle-withdraw-2"); code != http.StatusConflict || !strings.Contains(body, "CANDIDATE_UNAVAILABLE") {
		t.Fatalf("second withdrawal: %d %s", code, body)
	}
	if code, _, _ := e.as(harness, http.MethodDelete, "/admin/candidates/999", "", "Idempotency-Key", "lifecycle-withdraw-3"); code != http.StatusNotFound {
		t.Fatalf("withdrawing an unknown candidate: %d", code)
	}
	_, body, _ = e.as(harness, http.MethodGet, path, "")
	view := fromJSON[store.CandidateRecord](t, body)
	if view.Status != "withdrawn" || view.Checksum != first.Candidate.Checksum {
		t.Fatalf("withdrawn candidate: %s", body)
	}
	if s := e.status(); s.Revision != 1 {
		t.Fatalf("candidate lifecycle changed the revision to %d", s.Revision)
	}
}
