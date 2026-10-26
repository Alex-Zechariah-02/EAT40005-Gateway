package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// stagedCandidate is the reply to a candidate submission.
type stagedCandidate struct {
	Receipt   Outcome `json:"receipt"`
	Candidate struct {
		candidateRecord
		Active *bool `json:"active"`
	} `json:"candidate"`
}

// stage submits a complete policy candidate as an administrator.
func (e *env) stage(s adminSession, policy any, ifMatch, key string) (int, stagedCandidate, string) {
	e.t.Helper()
	body := string(mustJSON(map[string]any{"policy": policy}))
	code, data, _ := e.as(s, http.MethodPost, "/admin/candidates", body, "If-Match", ifMatch, "Idempotency-Key", key)
	var reply stagedCandidate
	json.Unmarshal([]byte(data), &reply)
	return code, reply, data
}

// establish asks for a genesis or recovery base from a staged candidate.
func (e *env) establish(s adminSession, path string, candidate int64, checksum string) (int, Outcome) {
	e.t.Helper()
	body := string(mustJSON(map[string]any{"candidate": candidate, "checksum": checksum}))
	code, data, _ := e.as(s, http.MethodPost, path, body)
	var outcome Outcome
	json.Unmarshal([]byte(data), &outcome)
	return code, outcome
}

// bootstrapped installs a gateway, enrols root and stages a deny-all
// candidate, all on the given binary. It returns root's password and the
// staged candidate.
func bootstrapped(t *testing.T, binary string) (*env, string, candidateRecord) {
	t.Helper()
	e, secret := installEnv(t, binary)
	password := testPassword()
	if code, body := e.enrol("root", secret, password); code != http.StatusCreated {
		t.Fatalf("enrolment: %d %s", code, body)
	}
	root := e.mustLogin("root", password)
	code, staged, body := e.stage(root, e.denyAllPolicy(), `"r0"`, "bootstrap-deny-all")
	if code != http.StatusCreated || !staged.Candidate.DenyAll {
		t.Fatalf("staging the deny-all candidate: %d %s", code, body)
	}
	return e, password, staged.Candidate.candidateRecord
}

// publishedEvents returns the transition.published events of one transition.
func publishedEvents(events []map[string]any, transition string) []map[string]any {
	var found []map[string]any
	for _, ev := range events {
		if ev["kind"] == "transition.published" && ev["transition"] == transition {
			found = append(found, ev)
		}
	}
	return found
}

// Genesis accepts only a validated deny-all
// candidate whose checksum the administrator confirms, exactly once, and
// leaves the gateway serving a policy that admits nothing.
func TestGenesisEstablishesDenyAllBase(t *testing.T) {
	e, password, denyAll := bootstrapped(t, binaries.plain)
	root := e.mustLogin("root", password)

	// Refusals before genesis: nothing is established and the mode stays.
	code, permissive, body := e.stage(root, e.protectedPolicy(alice), `"r0"`, "genesis-permissive")
	if code != http.StatusCreated || permissive.Candidate.DenyAll {
		t.Fatalf("staging a permissive candidate: %d %s", code, body)
	}
	refusals := []struct {
		name      string
		path      string
		candidate int64
		checksum  string
		status    int
		reason    string
	}{
		{"not deny-all", "/admin/genesis", permissive.Candidate.ID, permissive.Candidate.Checksum, http.StatusBadRequest, "NOT_DENY_ALL"},
		{"wrong checksum", "/admin/genesis", denyAll.ID, permissive.Candidate.Checksum, http.StatusPreconditionFailed, "PRECONDITION_FAILED"},
		{"unknown candidate", "/admin/genesis", 999, denyAll.Checksum, http.StatusNotFound, "NOT_FOUND"},
		{"recovery base outside recovery", "/admin/recovery-base", denyAll.ID, denyAll.Checksum, http.StatusConflict, "INVALID_STATE"},
	}
	for _, r := range refusals {
		if code, outcome := e.establish(root, r.path, r.candidate, r.checksum); code != r.status || outcome.Reason != r.reason {
			t.Errorf("%s: %d %+v, want %d %s", r.name, code, outcome, r.status, r.reason)
		}
	}
	put := `{"status":"active","roles":[]}`
	code, body, _ = e.as(root, http.MethodPut, "/admin/subjects/alice", put, "If-Match", `"r0"`, "Idempotency-Key", "bootstrap-membership")
	if code != http.StatusConflict || !strings.Contains(body, "INVALID_STATE") {
		t.Errorf("membership change in bootstrap: %d %s", code, body)
	}
	if e.health() != modeBootstrap {
		t.Fatal("a refused request left bootstrap mode")
	}

	// Genesis.
	code, outcome := e.establish(root, "/admin/genesis", denyAll.ID, denyAll.Checksum)
	if code != http.StatusOK || outcome.Outcome != "committed" || outcome.Revision == 0 || outcome.Generation == "" {
		t.Fatalf("genesis: %d %+v", code, outcome)
	}
	if !strings.HasPrefix(outcome.Transition, "genesis:") {
		t.Fatalf("genesis transition %q", outcome.Transition)
	}
	if e.health() != modeNormal {
		t.Fatal("genesis did not set normal mode")
	}
	code, body, _ = e.as(root, http.MethodGet, "/admin/status", "")
	s := fromJSON[status](t, body)
	if code != http.StatusOK || s.State != modeNormal || s.Revision != outcome.Revision || s.Generation != outcome.Generation {
		t.Fatalf("status after genesis: %d %s", code, body)
	}
	code, body, header := e.as(root, http.MethodGet, "/admin/policy", "")
	revision := fromJSON[revisionInfo](t, body)
	if code != http.StatusOK || header.Get("ETag") != fmt.Sprintf(`"r%d"`, outcome.Revision) ||
		revision.Origin != "genesis" || revision.Actor != "root" || revision.Parent != 0 ||
		revision.Checksum != denyAll.Checksum {
		t.Fatalf("confirmed policy after genesis: %d %s", code, body)
	}
	_, body, _ = e.as(root, http.MethodGet, fmt.Sprintf("/admin/candidates/%d", denyAll.ID), "")
	if c := fromJSON[candidateRecord](t, body); c.Status != "established" {
		t.Fatalf("genesis candidate status %q", c.Status)
	}

	// The deny-all base admits nothing.
	for _, path := range []string{"/a/x", "/b/x", "/d"} {
		if code, _ := e.get(path); code != http.StatusForbidden {
			t.Errorf("GET %s under the genesis base: %d", path, code)
		}
	}
	e.assertReceipts(0, 0, 0)

	// Retry and reuse: the same request returns the recorded outcome, another
	// candidate under the genesis identity is refused.
	// The replay carries the recorded transition and revision; the generation
	// is not part of the durable record.
	code, again := e.establish(root, "/admin/genesis", denyAll.ID, denyAll.Checksum)
	if code != http.StatusOK || again.Transition != outcome.Transition || again.Outcome != "committed" || again.Revision != outcome.Revision {
		t.Fatalf("genesis replay: %d %+v, want %+v", code, again, outcome)
	}
	code, other, body := e.stage(root, e.denyAllPolicy(), fmt.Sprintf(`"r%d"`, outcome.Revision), "after-genesis-deny-all")
	if code != http.StatusCreated {
		t.Fatalf("staging after genesis: %d %s", code, body)
	}
	if code, again := e.establish(root, "/admin/genesis", other.Candidate.ID, other.Candidate.Checksum); code != http.StatusUnprocessableEntity || again.Reason != "TRANSITION_ID_REUSED" {
		t.Fatalf("genesis with another candidate: %d %+v", code, again)
	}
	if code, again := e.establish(root, "/admin/recovery-base", other.Candidate.ID, other.Candidate.Checksum); code != http.StatusConflict || again.Reason != "INVALID_STATE" {
		t.Fatalf("recovery base in normal mode: %d %+v", code, again)
	}
	code, body, _ = e.as(root, http.MethodDelete, fmt.Sprintf("/admin/candidates/%d", denyAll.ID), "", "Idempotency-Key", "withdraw-established")
	if code != http.StatusConflict || !strings.Contains(body, "CANDIDATE_UNAVAILABLE") {
		t.Fatalf("withdrawing the established candidate: %d %s", code, body)
	}

	// History and audit agree with the response.
	code, body, _ = e.as(root, http.MethodGet, "/admin/history/"+outcome.Transition, "")
	entry := fromJSON[historyEntry](t, body)
	if code != http.StatusOK || entry.Actor != "root" || entry.Outcome != "committed" || entry.Revision != outcome.Revision ||
		entry.PublishedBy != "publisher" || entry.AuditedAt == nil {
		t.Fatalf("genesis history: %d %s", code, body)
	}
	events := e.eventsWhen(func(events []map[string]any) bool { return len(publishedEvents(events, outcome.Transition)) > 0 })
	published := publishedEvents(events, outcome.Transition)
	if len(published) != 1 || published[0]["actor"] != "root" || published[0]["generation"] != outcome.Generation {
		t.Fatalf("published events for genesis: %v", published)
	}

	e.stop()
	if n := queryInt(t, e.database, `SELECT COUNT(*) FROM revision`); n != 1 {
		t.Fatalf("%d revisions, want 1", n)
	}
	if n := queryInt(t, e.database, `SELECT COUNT(*) FROM transition WHERE kind = 'genesis'`); n != 1 {
		t.Fatalf("%d genesis transitions, want 1 (refusals under the genesis identity are not recorded)", n)
	}
}

// A crash during genesis after the commit, or after the outcome
// event, ends with one genesis revision and one published event, and the
// administrator's retry returns the recorded result.
func TestGenesisSurvivesCrash(t *testing.T) {
	for _, tc := range []struct {
		point       string
		publishedBy string
		reconciled  bool
	}{
		{"after-commit", "reconciliation", true},
		{"after-outcome", "publisher", false},
	} {
		t.Run(tc.point, func(t *testing.T) {
			e, password, denyAll := bootstrapped(t, binaries.plain)
			// after-outcome also fires on candidate staging, so the fault build
			// runs only for the genesis request.
			e.start(binaries.fault, "GATEWAY_FAULT="+tc.point)
			root := e.mustLogin("root", password)
			if code, outcome := e.establish(root, "/admin/genesis", denyAll.ID, denyAll.Checksum); code != 0 {
				t.Fatalf("genesis answered %d instead of crashing: %+v", code, outcome)
			}
			if code := e.waitExit(); code != crashExit {
				t.Fatalf("exit code %d", code)
			}

			e.start(binaries.plain)
			if e.health() != modeNormal {
				t.Fatal("the committed genesis is not serving after restart")
			}
			root = e.mustLogin("root", password)
			code, outcome := e.establish(root, "/admin/genesis", denyAll.ID, denyAll.Checksum)
			if code != http.StatusOK || outcome.Outcome != "committed" || outcome.Revision != 1 {
				t.Fatalf("retry after the crash: %d %+v", code, outcome)
			}
			_, body, _ := e.as(root, http.MethodGet, "/admin/history/"+outcome.Transition, "")
			if entry := fromJSON[historyEntry](t, body); entry.PublishedBy != tc.publishedBy || entry.AuditedAt == nil {
				t.Fatalf("history after the crash: %s", body)
			}
			if code, _ := e.get("/a/x"); code != http.StatusForbidden {
				t.Fatalf("data plane after recovery: %d", code)
			}
			e.assertReceipts(0, 0, 0)

			published := publishedEvents(e.auditEvents(), outcome.Transition)
			if len(published) != 1 || (published[0]["reason"] == "reconciled") != tc.reconciled {
				t.Fatalf("published events for genesis: %v", published)
			}
			e.stop()
			if n := queryInt(t, e.database, `SELECT COUNT(*) FROM revision`); n != 1 {
				t.Fatalf("%d revisions, want 1", n)
			}
		})
	}
}
