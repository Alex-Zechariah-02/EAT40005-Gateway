// Refused activation requests.

package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

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
	crafted := map[string]policy.Policy{}
	duplicateRoute := e.cpaCandidate()
	duplicateRoute.Operations = append(duplicateRoute.Operations, policy.Operation{ID: "read-a-again", API: "b", Method: "GET", Target: "/a/*", Access: "allow", Auth: "none"})
	crafted["DUPLICATE_OPERATION"] = duplicateRoute
	unknownRole := e.cpaCandidate()
	unknownRole.Permits = append(unknownRole.Permits, policy.Permit{Role: "ADMIN", Operation: "read-a"})
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
