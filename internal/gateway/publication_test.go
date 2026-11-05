// Candidate publication: refusals, concurrency and stale revisions.

package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

func TestInvalidCandidateLeavesStateUnchanged(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.policy(e.a, false), seedOptions{})
	e.start(binaries.fault)
	before := e.status()

	withOperation := func(op policy.Operation) json.RawMessage {
		p := e.policy(e.a, false)
		p.Operations = append(p.Operations, op)
		return mustJSON(p)
	}
	withUpstream := func(dial string) json.RawMessage {
		p := e.policy(e.a, false)
		p.Upstreams = append(p.Upstreams, policy.Upstream{ID: "X", Dial: dial})
		return mustJSON(p)
	}
	cases := []struct {
		name   string
		change Change
		status int
		reason string
	}{
		{"duplicate operation", Change{Policy: withOperation(policy.Operation{ID: "dup", API: "a", Method: "GET", Target: "/a/*", Access: "allow", Auth: "none"})}, 400, "DUPLICATE_OPERATION"},
		{"unknown upstream", Change{Policy: withOperation(policy.Operation{ID: "u", API: "z", Method: "GET", Target: "/u", Access: "allow", Auth: "none"})}, 400, "UNKNOWN_REFERENCE"},
		{"placeholder dial", Change{Policy: withUpstream("{http.request.header.x}:80")}, 400, "INVALID_UPSTREAM"},
		{"non-loopback dial", Change{Policy: withUpstream("10.0.0.5:80")}, 400, "INVALID_UPSTREAM"},
		{"hostname dial", Change{Policy: withUpstream("localhost:80")}, 400, "INVALID_UPSTREAM"},
		{"non-canonical target", Change{Policy: withOperation(policy.Operation{ID: "n", API: "a", Method: "GET", Target: "/a/../b", Access: "allow", Auth: "none"})}, 400, "INVALID_TARGET"},
		{"invalid method", Change{Policy: withOperation(policy.Operation{ID: "m", API: "a", Method: "get", Target: "/m", Access: "allow", Auth: "none"})}, 400, "INVALID_METHOD"},
		{"unknown field", Change{Policy: json.RawMessage(`{"schema_version":4,"profile":"core","extra":1}`)}, 400, "UNKNOWN_FIELD"},
		{"separate rate rule list", Change{Policy: json.RawMessage(`{"schema_version":4,"profile":"core","rate_rules":[{"id":"r","count":1}]}`)}, 400, "UNKNOWN_FIELD"},
		// An invalid rate rule or schema rejects the whole candidate; nothing
		// else in it is published.
		{"body without a schema", Change{Policy: withOperation(policy.Operation{ID: "x", API: "a", Method: "POST", Target: "/x", Access: "allow", Auth: "none", Body: &policy.BodyRule{MaxBytes: 10}})}, 400, "INVALID_VALUE"},
		{"schema with an unsupported keyword", Change{Policy: withOperation(policy.Operation{ID: "x", API: "a", Method: "POST", Target: "/x", Access: "allow", Auth: "none",
			Body: jsonBody(10, `{"type":"object","additionalProperties":true,"patternProperties":{}}`)})}, 400, "UNSUPPORTED_FEATURE"},
		{"rate rule with zero count", Change{Policy: withOperation(policy.Operation{ID: "x", API: "a", Method: "GET", Target: "/x", Access: "allow", Auth: "required",
			Rate: &policy.RateRule{Algorithm: "sliding-window", Count: 0, WindowSeconds: 60, Key: "subject"}})}, 400, "INVALID_VALUE"},
		{"rate rule on a public operation", Change{Policy: withOperation(policy.Operation{ID: "x", API: "a", Method: "GET", Target: "/x", Access: "allow", Auth: "none",
			Rate: &policy.RateRule{Algorithm: "sliding-window", Count: 5, WindowSeconds: 60, Key: "subject"}})}, 400, "INVALID_VALUE"},
		{"stale base", Change{Expected: 7, Policy: mustJSON(e.policy(e.a, true))}, 409, "STALE"},
		{"unsupported schema version", Change{Policy: json.RawMessage(`{"schema_version":1,"profile":"routing-only"}`)}, 400, "SCHEMA_VERSION_UNSUPPORTED"},
		{"previous schema version", Change{Policy: json.RawMessage(`{"schema_version":2,"profile":"jwt-rbac"}`)}, 400, "SCHEMA_VERSION_UNSUPPORTED"},
		{"profile without authentication", Change{Policy: withProfile(e.policy(e.a, true), "routing-only")}, 400, "UNSUPPORTED_FEATURE"},
		{"profile claiming full enforcement", Change{Policy: withProfile(e.policy(e.a, true), "protected")}, 400, "UNSUPPORTED_FEATURE"},
		{"permit for unknown role", Change{Policy: withPermit(e.policy(e.a, true), policy.Permit{Role: "ADMIN", Operation: "write-a"})}, 400, "UNKNOWN_REFERENCE"},
		{"subject with unknown role", Change{Policy: withSubject(e.policy(e.a, true), Subject{ID: "s-1", Status: "active", Roles: []string{"ADMIN"}})}, 400, "UNKNOWN_FIELD"},
		{"subject with unknown status", Change{Policy: withSubject(e.policy(e.a, true), Subject{ID: "s-1", Status: "suspended", Roles: []string{"STAFF"}})}, 400, "UNKNOWN_FIELD"},
		{"subject with invalid identifier", Change{Policy: withSubject(e.policy(e.a, true), Subject{ID: "s 1", Status: "active", Roles: []string{"STAFF"}})}, 400, "UNKNOWN_FIELD"},
	}
	for i, tc := range cases {
		tc.change.ID = fmt.Sprintf("invalid-%d", i)
		if tc.change.Expected == 0 {
			tc.change.Expected = 1
		}
		outcome, code := e.publish(tc.change)
		if code != tc.status || outcome.Reason != tc.reason {
			t.Errorf("%s: %d %+v, want %d %s", tc.name, code, outcome, tc.status, tc.reason)
		}
		if now := e.status(); now.Generation != before.Generation || now.Revision != before.Revision {
			t.Fatalf("%s changed the published state to %+v", tc.name, now)
		}
	}

	// A repeated member anywhere in the request makes the request itself
	// malformed: it is refused before any transition is recorded.
	malformed := `{"transition_id":"dup","expected_revision":1,"policy":{"schema_version":4,"schema_version":4,"profile":"core"}}`
	if code, body := e.adminRaw(http.MethodPost, "/test/publish", malformed); code != http.StatusBadRequest || !strings.Contains(body, "bad_request") {
		t.Errorf("repeated member: %d %s", code, body)
	}

	oversize := Change{ID: "oversize", Expected: 1, Policy: json.RawMessage(`{"upstreams":[],"operations":[],"pad":"` + strings.Repeat("x", 70000) + `"}`)}
	if _, code := e.publish(oversize); code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize change: %d", code)
	}
	// The hook cannot create a provisional base: that needs an activation
	// through the confirmed-activation lifecycle, whose period bounds are
	// tested there.
	provisional := `{"transition_id":"provisional","expected_revision":1,"provisional_seconds":60,"policy":` + string(mustJSON(e.policy(e.a, true))) + `}`
	if code, body := e.adminRaw(http.MethodPost, "/test/publish", provisional); code != http.StatusBadRequest || !strings.Contains(body, "bad_request") {
		t.Errorf("provisional publication through the hook: %d %s", code, body)
	}

	if code, _ := e.get("/a/x"); code != http.StatusOK {
		t.Fatalf("routing after rejections: %d", code)
	}
	e.assertReceipts(1, 0, 0)
	if g := e.a.receipts()[0].Generation; g != before.Generation {
		t.Fatalf("routing used %s, want %s", g, before.Generation)
	}

	outcome, code := e.publish(Change{ID: "valid", Expected: 1, Policy: mustJSON(e.policy(e.a, true))})
	if code != http.StatusOK || outcome.Revision != 2 {
		t.Fatalf("valid change: %d %+v", code, outcome)
	}

	e.stop()
	db := inspect(t, e.database)
	if n := count(t, db, `SELECT COUNT(*) FROM revision`); n != 2 {
		t.Fatalf("%d revisions stored, want 2", n)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM transition WHERE outcome = 'rejected'`); n != len(cases) {
		t.Fatalf("%d rejections recorded, want %d", n, len(cases))
	}
}

func withProfile(p policy.Policy, profile string) json.RawMessage {
	p.Profile = profile
	return mustJSON(p)
}

func withPermit(p policy.Policy, permit policy.Permit) json.RawMessage {
	p.Roles = append(p.Roles, policy.Role{ID: "STAFF"})
	p.Operations[1].Auth = "required"
	p.Permits = append(p.Permits, permit)
	return mustJSON(p)
}

func withSubject(p policy.Policy, s Subject) json.RawMessage {
	var document map[string]any
	json.Unmarshal(mustJSON(p), &document)
	document["subjects"] = []Subject{s}
	return mustJSON(document)
}

func TestConcurrentAndStalePublications(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.policy(e.a, false), seedOptions{})
	e.start(binaries.fault)

	type result struct {
		outcome Outcome
		code    int
	}
	results := make([]result, 2)
	policies := []policy.Policy{e.policy(e.a, true), e.policy(e.b, false)}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range results {
		wg.Go(func() {
			<-start
			outcome, code := e.publish(Change{ID: fmt.Sprintf("race-%d", i), Expected: 1, Policy: mustJSON(policies[i])})
			results[i] = result{outcome, code}
		})
	}
	close(start)
	wg.Wait()

	winner := -1
	for i, r := range results {
		switch {
		case r.code == http.StatusOK && winner == -1:
			winner = i
		case r.code == http.StatusConflict && r.outcome.Reason == "STALE":
		default:
			t.Fatalf("unexpected outcomes %+v", results)
		}
	}
	if winner == -1 {
		t.Fatalf("no publication succeeded: %+v", results)
	}
	committed := results[winner].outcome

	replay, code := e.publish(Change{ID: committed.Transition, Expected: 1, Policy: mustJSON(policies[winner])})
	if code != http.StatusOK || replay.Revision != committed.Revision {
		t.Fatalf("replay: %d %+v, want recorded revision %d", code, replay, committed.Revision)
	}
	reused, code := e.publish(Change{ID: committed.Transition, Expected: 2, Policy: mustJSON(policies[1-winner])})
	if code != http.StatusUnprocessableEntity || reused.Reason != "TRANSITION_ID_REUSED" {
		t.Fatalf("reused identity: %d %+v", code, reused)
	}
	if s := e.status(); s.Revision != committed.Revision || s.Generation != committed.Generation {
		t.Fatalf("published %+v, want %+v", s, committed)
	}

	e.stop()
	db := inspect(t, e.database)
	if n := count(t, db, `SELECT COUNT(*) FROM transition WHERE outcome = 'committed'`); n != 1 {
		t.Fatalf("%d committed transitions, want 1", n)
	}
	if n := count(t, db, `SELECT confirmed FROM base`); int64(n) != committed.Revision {
		t.Fatalf("confirmed revision %d, want %d", n, committed.Revision)
	}
}
