package gateway

import (
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// These tests cover temporary overrides together with confirmed policy
// activation, and their behaviour across crashes, restarts, clock changes,
// damaged state, audit failures and a declared historical restore, through
// real gateway processes.

// upstreamCandidate moves API a from upstream A to upstream C and changes
// nothing else, so it touches no target an override in these tests relies on.
func (e *env) upstreamCandidate() Policy {
	p := e.protectedPolicy(alice, bob)
	p.Upstreams = append(p.Upstreams, Upstream{ID: "C", Dial: e.c.addr})
	p.APIs[0].Upstream = "C"
	return p
}

// A candidate that would change or invalidate an active override is refused
// at activation, naming the override, even when it was staged before the
// override existed; once the override is gone the same candidate activates.
// A candidate that touches no override's targets becomes provisional and then
// confirmed with the overrides still applied.
func TestCandidateAgainstActiveOverrides(t *testing.T) {
	e := newEnv(t)
	basePolicy := e.protectedPolicy(alice, bob)
	basePolicy.Operations[0].Rate = rate(4, 60) // read-a
	seed(t, e.database, basePolicy, seedOptions{})
	e.start(binaries.plain)
	harness := e.mustLogin(harnessAdmin, harnessPassword)

	// Staged before any override exists: grants READER the write permanently.
	grantCandidate := e.cpaCandidate()
	grantCandidate.Operations[0].Rate = rate(4, 60)
	code, early, body := e.stage(harness, grantCandidate, `"r1"`, "stage-before-override")
	if code != http.StatusCreated {
		t.Fatalf("staging: %d %s", code, body)
	}
	grant := e.mustOverride(harness, permitOverride(overrideException, "READER", "write-a", 600), `"r1"`, "candidate-grant-0001")
	raise := e.mustOverride(harness, rateOverride(overrideException, "read-a", 8, 600), `"r1"`, "candidate-raise-0001")
	closeB := e.mustOverride(harness, operationContainment("read-b", 600), `"r1"`, "candidate-close-0001")
	before := e.status()

	refused := map[string]struct {
		policy Policy
		holder int64
	}{}
	refused["same permit as an override"] = struct {
		policy Policy
		holder int64
	}{grantCandidate, grant.Override}
	newCount := e.protectedPolicy(alice, bob)
	newCount.Operations[0].Rate = rate(6, 60)
	refused["rate count of an overridden rule"] = struct {
		policy Policy
		holder int64
	}{newCount, raise.Override}
	noReader := e.protectedPolicy(alice, Subject{ID: "bob", Status: "active", Roles: []string{}})
	noReader.Operations[0].Rate = rate(4, 60)
	noReader.Roles = []Role{{ID: "WRITER"}}
	noReader.Permits = slices.DeleteFunc(noReader.Permits, func(p Permit) bool { return p.Role == "READER" })
	refused["role an override grants removed"] = struct {
		policy Policy
		holder int64
	}{noReader, grant.Override}
	narrower := e.protectedPolicy(alice, bob)
	narrower.Operations[0].Rate = rate(4, 60)
	narrower.Operations = append(narrower.Operations, Operation{ID: "special-a", API: "a", Method: "GET", Target: "/a/special", Access: "allow", Auth: "none"})
	refused["more specific route inside an overridden prefix"] = struct {
		policy Policy
		holder int64
	}{narrower, raise.Override}
	reRouted := e.protectedPolicy(alice, bob)
	reRouted.Operations[0].Rate = rate(4, 60)
	reRouted.Operations[3].Target = "/bee/*"
	refused["contained operation moved"] = struct {
		policy Policy
		holder int64
	}{reRouted, closeB.Override}

	i := 0
	for name, r := range refused {
		staged := early.Candidate.candidateRecord
		if name != "same permit as an override" {
			code, s, body := e.stage(harness, r.policy, `"r1"`, fmt.Sprintf("stage-conflict-%02d-xxxx", i))
			if code != http.StatusCreated {
				t.Fatalf("%s: staging %d %s", name, code, body)
			}
			staged = s.Candidate.candidateRecord
		}
		code, outcome, body := e.activate(harness, staged, 60, `"r1"`, fmt.Sprintf("activate-conflict-%02d-x", i))
		if code != http.StatusConflict || outcome.Reason != "OVERRIDE_CONFLICT" || outcome.Override != r.holder {
			t.Errorf("%s: %d %s, want OVERRIDE_CONFLICT with override %d", name, code, body, r.holder)
		}
		i++
	}
	if s := e.status(); s.Generation != before.Generation || s.Provisional != 0 {
		t.Fatalf("refused activations changed the status: %+v", s)
	}
	if n := len(e.activeOverrides(harness).Overrides); n != 3 {
		t.Fatalf("%d active overrides after the refusals", n)
	}

	// The grant ends; the candidate staged before it now activates, because
	// activation checks the overrides active at that moment.
	if code, _, body := e.revokeOverride(harness, grant.Override, "candidate-grant-revoke"); code != http.StatusOK {
		t.Fatalf("revocation: %d %s", code, body)
	}
	code, activated, body := e.activate(harness, early.Candidate.candidateRecord, 60, `"r1"`, "activate-after-revoke")
	if code != http.StatusOK || !strings.HasPrefix(activated.Generation, fmt.Sprintf("r2+o%d.%d-", raise.Override, closeB.Override)) {
		t.Fatalf("activation after the revocation: %d %s", code, body)
	}
	// Enforcement is the candidate (bob writes /a, served by C) with the
	// remaining overrides (/b closed; reads of /a limited to 8, not 4).
	e.resetReceivers()
	if e.probe("bob", http.MethodPost, "/a/item") != http.StatusOK || e.probe("bob", http.MethodGet, "/b/x") != http.StatusForbidden {
		t.Fatal("provisional enforcement")
	}
	if got := e.sendAs("alice", http.MethodGet, "/a/x", "", 9); !statusesAre(got, append(times(8, 200), 429)...) {
		t.Fatalf("alice reads under the provisional base with the rate override: %v", got)
	}
	e.assertReceipts(0, 0, 9)

	// Confirmation keeps the overrides and does not remove or expire any.
	code, confirmed, body := e.resolve(harness, "confirm", activated.Provisional, `"r1"`, "confirm-with-overrides")
	if code != http.StatusOK || !strings.HasPrefix(confirmed.Generation, fmt.Sprintf("r2+o%d.%d-", raise.Override, closeB.Override)) {
		t.Fatalf("confirmation: %d %s", code, body)
	}
	view := e.activeOverrides(harness)
	if view.Confirmed != 2 || view.Provisional != 0 || len(view.Overrides) != 2 || view.Generation != confirmed.Generation {
		t.Fatalf("overrides after the confirmation %+v", view)
	}
	e.resetReceivers()
	if e.probe("bob", http.MethodPost, "/a/item") != http.StatusOK || e.probe("bob", http.MethodGet, "/b/x") != http.StatusForbidden {
		t.Fatal("enforcement after the confirmation")
	}
	e.assertReceipts(0, 0, 1)
	events := e.eventsWhen(func(events []map[string]any) bool { return countKind(events, "cpa.rejected") >= len(refused) })
	conflicts := 0
	for _, ev := range events {
		if ev["kind"] == "cpa.rejected" && ev["reason"] == "OVERRIDE_CONFLICT" && ev["override"] != nil {
			conflicts++
		}
	}
	if conflicts != len(refused) {
		t.Fatalf("%d conflict refusal events, want %d", conflicts, len(refused))
	}
}

// While a provisional base is pending, no override can be created, extended
// or broadened, but overrides can still be revoked and still expire, without
// confirming anything. Rollback and timeout restore the earlier confirmed
// base with the overrides still active at that moment, never an earlier
// effective policy, and an override that ended stays ended.
func TestProvisionalRestrictsOverrides(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(alice, bob), seedOptions{})
	e.start(binaries.plain)
	harness := e.mustLogin(harnessAdmin, harnessPassword)

	closeB := e.mustOverride(harness, operationContainment("read-b", 600), `"r1"`, "provisional-close-01")
	grant := e.mustOverride(harness, permitOverride(overrideException, "READER", "write-a", 4), `"r1"`, "provisional-grant-01")
	limitAlice := e.mustOverride(harness, permitOverride(overrideContainment, "WRITER", "read-a", 600), `"r1"`, "provisional-limit-01")
	earlierGenerations := map[string]bool{e.status().Generation: true}

	_, activated := e.provisionalFrom(harness, e.upstreamCandidate(), 1, 60, "provisional-restrict")
	if !strings.HasPrefix(activated.Generation, fmt.Sprintf("r2+o%d.%d.%d-", closeB.Override, grant.Override, limitAlice.Override)) {
		t.Fatalf("provisional generation %s", activated.Generation)
	}
	e.resetReceivers()
	if e.probe("bob", http.MethodPost, "/a/item") != http.StatusOK || e.probe("bob", http.MethodGet, "/b/x") != http.StatusForbidden ||
		e.probe("alice", http.MethodGet, "/a/x") != http.StatusForbidden {
		t.Fatal("provisional enforcement with overrides")
	}
	e.assertReceipts(0, 0, 1)

	// No creation, extension or broadening while provisional.
	for i, r := range []OverrideRequest{
		operationContainment("write-a", 600),
		permitOverride(overrideException, "READER", "write-a", 3600), // same target, longer: an extension
		rateOverride(overrideException, "read-a", 100, 60),           // a broadening
		permitOverride(overrideContainment, "READER", "read-a", 60),  // even a restriction
	} {
		code, outcome, body := e.createOverride(harness, r, `"r1"`, fmt.Sprintf("provisional-create-%02d", i))
		if code != http.StatusConflict || outcome.Reason != "PROVISIONAL_ACTIVE" {
			t.Errorf("creation %d while provisional: %d %s", i, code, body)
		}
	}
	if code, body, _ := e.as(harness, http.MethodPost, "/admin/overrides/validate", string(mustJSON(operationContainment("write-a", 60))), "If-Match", `"r1"`); code != http.StatusConflict ||
		!strings.Contains(body, "PROVISIONAL_ACTIVE") {
		t.Errorf("validation while provisional: %d %s", code, body)
	}

	// The short grant expires while provisional: the base stays provisional.
	waitFor(t, grant.Expires.Add(5*time.Second), "the expiry while provisional", func() bool { return len(e.status().Overrides) == 2 })
	s := e.status()
	if s.Provisional != activated.Provisional || s.Confirmed != 1 {
		t.Fatalf("status after the expiry %+v", s)
	}
	earlierGenerations[s.Generation] = true
	if e.probe("bob", http.MethodPost, "/a/item") != http.StatusForbidden {
		t.Fatal("expired grant still enforced while provisional")
	}
	// A revocation while provisional.
	code, revoked, body := e.revokeOverride(harness, closeB.Override, "provisional-revoke-01")
	if code != http.StatusOK || revoked.Revision != activated.Provisional || !strings.HasPrefix(revoked.Generation, fmt.Sprintf("r2+o%d-", limitAlice.Override)) {
		t.Fatalf("revocation while provisional: %d %s", code, body)
	}
	if s := e.status(); s.Provisional != activated.Provisional {
		t.Fatalf("the revocation ended the provisional base: %+v", s)
	}
	e.resetReceivers()
	if e.probe("bob", http.MethodGet, "/b/x") != http.StatusOK {
		t.Fatal("revoked containment still enforced")
	}
	e.assertReceipts(0, 1, 0)

	// Explicit rollback: the confirmed base with the one override left.
	code, rolledBack, body := e.resolve(harness, "rollback", activated.Provisional, `"r1"`, "provisional-rollback-1")
	if code != http.StatusOK || rolledBack.Revision != 1 || !strings.HasPrefix(rolledBack.Generation, fmt.Sprintf("r1+o%d-", limitAlice.Override)) ||
		earlierGenerations[rolledBack.Generation] {
		t.Fatalf("rollback: %d %s (earlier generations %v)", code, body, earlierGenerations)
	}
	e.resetReceivers()
	if e.probe("bob", http.MethodPost, "/a/item") != http.StatusForbidden || e.probe("bob", http.MethodGet, "/b/x") != http.StatusOK ||
		e.probe("alice", http.MethodGet, "/a/x") != http.StatusForbidden || e.probe("bob", http.MethodGet, "/a/x") != http.StatusOK {
		t.Fatal("enforcement after the rollback")
	}
	e.assertReceipts(1, 1, 0)

	// Timeout with an override that expired first: it is not revived.
	shortGrant := e.mustOverride(harness, permitOverride(overrideException, "READER", "write-a", 1), `"r1"`, "provisional-grant-02")
	_, timed := e.provisionalFrom(harness, e.upstreamCandidate(), 1, 3, "provisional-timeout")
	waitFor(t, timed.Deadline.Add(5*time.Second), "the timeout", func() bool { return e.status().Provisional == 0 })
	s = e.status()
	if s.Confirmed != 1 || !strings.HasPrefix(s.Generation, fmt.Sprintf("r1+o%d-", limitAlice.Override)) || !slices.Equal(s.Overrides, []int64{limitAlice.Override}) {
		t.Fatalf("status after the timeout %+v", s)
	}
	e.resetReceivers()
	if e.probe("bob", http.MethodPost, "/a/item") != http.StatusForbidden {
		t.Fatal("an expired grant was revived by the timeout")
	}
	e.assertReceipts(0, 0, 0)

	e.stop()
	for _, o := range storedOverrides(t, e.database) {
		want := map[int64]string{closeB.Override: overrideRevoked, grant.Override: overrideExpired, limitAlice.Override: overrideActive, shortGrant.Override: overrideExpired}[o.ID]
		if o.Status != want {
			t.Fatalf("override %d status %s, want %s", o.ID, o.Status, want)
		}
	}
	list := storedActivations(t, e.database)
	if len(list) != 2 || list[0].Disposition != dispositionRolledBack || list[1].Disposition != dispositionTimedOut {
		t.Fatalf("activations %+v", list)
	}
}

// An activation and an override creation sent at the same moment are
// decided in one order: if the activation is first, the creation is refused
// as provisional; if the creation is first, the conflicting candidate is
// refused. Confirmation, rollback and an override's expiry racing at a
// provisional deadline give each its single final disposition.
func TestConcurrentOverridesAndActivation(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(alice, bob), seedOptions{})
	e.start(binaries.plain)
	harness := e.mustLogin(harnessAdmin, harnessPassword)

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

	orders := map[string]int{}
	for round := range 8 {
		_, staged, _ := e.stage(harness, e.cpaCandidate(), `"r1"`, fmt.Sprintf("race-stage-%02d-xxxx", round))
		var activation, creation Outcome
		var activationCode, creationCode int
		concurrently(
			func() {
				activationCode, activation, _ = e.activate(harness, staged.Candidate.candidateRecord, 60, `"r1"`, fmt.Sprintf("race-activate-%02d-xx", round))
			},
			func() {
				creationCode, creation, _ = e.createOverride(harness, permitOverride(overrideException, "READER", "write-a", 600), `"r1"`, fmt.Sprintf("race-create-%02d-xxxx", round))
			},
		)
		switch {
		case activationCode == http.StatusOK && creationCode == http.StatusConflict && creation.Reason == "PROVISIONAL_ACTIVE":
			orders["activation first"]++
			if code, _, body := e.resolve(harness, "rollback", activation.Provisional, `"r1"`, fmt.Sprintf("race-rollback-%02d-x", round)); code != http.StatusOK {
				t.Fatalf("rollback: %d %s", code, body)
			}
		case creationCode == http.StatusCreated && activationCode == http.StatusConflict && activation.Reason == "OVERRIDE_CONFLICT":
			orders["creation first"]++
			if code, _, body := e.revokeOverride(harness, creation.Override, fmt.Sprintf("race-revoke-%02d-xxx", round)); code != http.StatusOK {
				t.Fatalf("revocation: %d %s", code, body)
			}
		default:
			t.Fatalf("round %d: activation %d %+v, creation %d %+v", round, activationCode, activation, creationCode, creation)
		}
		if s := e.status(); s.Provisional != 0 || len(s.Overrides) != 0 || s.Confirmed != 1 {
			t.Fatalf("round %d left %+v", round, s)
		}
	}
	t.Logf("serial orders observed: %v", orders)

	// Confirmation, rollback and an override expiry at a 2 s deadline.
	confirmed := int64(1)
	for round := range 4 {
		o := e.mustOverride(harness, operationContainment("read-b", 2), policyETag(confirmed), fmt.Sprintf("race-expiring-%02d-x", round))
		_, activated := e.provisionalFrom(harness, e.upstreamCandidate(), confirmed, 2, fmt.Sprintf("race-deadline-%02d", round))
		if round%2 == 1 {
			// The other candidate: back to the protected policy.
			e.resolve(harness, "rollback", activated.Provisional, policyETag(confirmed), fmt.Sprintf("race-reset-%02d-xxxxx", round))
			_, activated = e.provisionalFrom(harness, e.protectedPolicy(alice, bob), confirmed, 2, fmt.Sprintf("race-deadline-b-%02d", round))
		}
		time.Sleep(time.Until(o.Expires.Add(-15 * time.Millisecond)))
		var codes [3]int
		var outcomes [3]Outcome
		concurrently(
			func() {
				codes[0], outcomes[0], _ = e.resolve(harness, "confirm", activated.Provisional, policyETag(confirmed), fmt.Sprintf("race-confirm-%02d-xxx", round))
			},
			func() {
				codes[1], outcomes[1], _ = e.resolve(harness, "rollback", activated.Provisional, policyETag(confirmed), fmt.Sprintf("race-rollback-%02d-xx", round))
			},
			func() {
				codes[2], outcomes[2], _ = e.revokeOverride(harness, o.Override, fmt.Sprintf("race-revoke-%02d-xxxx", round))
			},
		)
		waitFor(t, time.Now().Add(5*time.Second), "the provisional base and override to end", func() bool {
			s := e.status()
			return s.Provisional == 0 && len(s.Overrides) == 0
		})
		s := e.status()
		if s.Confirmed == activated.Provisional {
			confirmed = activated.Provisional
		}
		t.Logf("round %d: confirm %d %s, rollback %d %s, revoke %d %s", round, codes[0], outcomes[0].Reason+outcomes[0].Disposition,
			codes[1], outcomes[1].Reason+outcomes[1].Disposition, codes[2], outcomes[2].Reason+outcomes[2].Disposition)
	}

	e.stop()
	for _, a := range storedActivations(t, e.database) {
		n := queryInt(t, e.database, `SELECT COUNT(*) FROM transition WHERE kind IN ('cpa.confirm', 'cpa.rollback', 'cpa.timeout')
			AND outcome = 'committed' AND result_candidate = ?`, a.Candidate)
		if a.Disposition == "" || n != 1 {
			t.Fatalf("activation %+v has %d committed dispositions", a, n)
		}
	}
	for _, o := range storedOverrides(t, e.database) {
		if o.Status == overrideActive {
			t.Fatalf("override %+v still active", o)
		}
		if n := queryInt(t, e.database, `SELECT COUNT(*) FROM transition WHERE outcome = 'committed' AND kind IN ('tpo.revoke', 'tpo.expire') AND id = ?`, o.Resolution); n != 1 {
			t.Fatalf("override %d has %d ending transitions", o.ID, n)
		}
	}
}

// A rate override applies to the provisional base too and keeps the
// counters across activation and rollback; an override that expired while
// the base was provisional is not restored by the rollback. Decisions are
// worked out by hand from the admission rule at a controlled limiter time.
func TestRateOverrideAcrossActivation(t *testing.T) {
	ratePolicy := func(e *env) Policy {
		p := e.protectedPolicy(alice, bob)
		p.Operations[0].Rate = rate(3, 60) // read-a: 3 per minute per subject
		return p
	}
	e, clock := newRateEnv(t, ratePolicy, base)
	harness := e.mustLogin(harnessAdmin, harnessPassword)
	candidate := e.upstreamCandidate()
	candidate.Operations[0].Rate = rate(3, 60)

	if got := e.sendAs("alice", http.MethodGet, "/a/x", "", 2); !statusesAre(got, 200, 200) {
		t.Fatalf("alice under the base: %v", got)
	}
	raise := e.mustOverride(harness, rateOverride(overrideException, "read-a", 5, 600), `"r1"`, "rate-cpa-raise-0001")
	_, activated := e.provisionalFrom(harness, candidate, 1, 60, "rate-cpa-activate")
	// C=2 under the provisional base with the exception to 5: three more.
	if got := e.sendAs("alice", http.MethodGet, "/a/x", "", 4); !statusesAre(got, 200, 200, 200, 429) {
		t.Fatalf("alice while provisional: %v", got)
	}
	if code, rolledBack, body := e.resolve(harness, "rollback", activated.Provisional, `"r1"`, "rate-cpa-rollback-1"); code != http.StatusOK ||
		!strings.HasPrefix(rolledBack.Generation, fmt.Sprintf("r1+o%d-", raise.Override)) {
		t.Fatalf("rollback: %d %s", code, body)
	}
	// Same counter, same count 5: still refused, nothing granted back.
	if got := e.sendAs("alice", http.MethodGet, "/a/x", "", 1); !statusesAre(got, 429) {
		t.Fatalf("alice after the rollback: %v", got)
	}
	if code, _, body := e.revokeOverride(harness, raise.Override, "rate-cpa-revoke-01"); code != http.StatusOK {
		t.Fatalf("revocation: %d %s", code, body)
	}

	// A fresh window; an exception that expires while provisional.
	clock.set(base + 180_000)
	short := e.mustOverride(harness, rateOverride(overrideException, "read-a", 5, 2), `"r1"`, "rate-cpa-short-0001")
	_, activated = e.provisionalFrom(harness, candidate, 1, 60, "rate-cpa-activate-2")
	if got := e.sendAs("alice", http.MethodGet, "/a/x", "", 4); !statusesAre(got, times(4, 200)...) {
		t.Fatalf("alice under the short exception: %v", got)
	}
	waitFor(t, short.Expires.Add(5*time.Second), "the expiry", func() bool { return len(e.status().Overrides) == 0 })
	// Base count 3 against C=4.
	if got := e.sendAs("alice", http.MethodGet, "/a/x", "", 1); !statusesAre(got, 429) {
		t.Fatalf("alice after the expiry: %v", got)
	}
	if code, rolledBack, body := e.resolve(harness, "rollback", activated.Provisional, `"r1"`, "rate-cpa-rollback-2"); code != http.StatusOK ||
		!strings.HasPrefix(rolledBack.Generation, "r1-") {
		t.Fatalf("rollback: %d %s", code, body)
	}
	if got := e.sendAs("alice", http.MethodGet, "/a/x", "", 1); !statusesAre(got, 429) {
		t.Fatalf("alice after the second rollback: %v", got)
	}
	// bob, never counted, has the base's 3.
	if got := e.sendAs("bob", http.MethodGet, "/a/x", "", 4); !statusesAre(got, 200, 200, 200, 429) {
		t.Fatalf("bob: %v", got)
	}
}

// A crash at each step of creating, revoking or expiring an override leaves
// either the state before it (crash before the commit) or the state after it
// (crash after the commit), which a restart serves, completes and audits
// once; a retried request is applied once.
func TestOverrideTransitionsSurviveCrash(t *testing.T) {
	type point struct {
		name      string
		committed bool
	}
	creation := []point{
		{"tpo-create-before-commit", false}, {"tpo-commit-partway", false}, {"tpo-create-after-commit", true},
		{"tpo-create-after-swap", true}, {"tpo-create-after-receipt", true}, {"after-outcome", true},
	}
	for _, p := range creation {
		t.Run("create/"+p.name, func(t *testing.T) {
			e := newEnv(t)
			seed(t, e.database, e.protectedPolicy(alice, bob), seedOptions{})
			e.start(binaries.fault, "GATEWAY_FAULT="+p.name)
			harness := e.mustLogin(harnessAdmin, harnessPassword)
			request := permitOverride(overrideException, "READER", "write-a", 600)
			if code, _, body := e.createOverride(harness, request, `"r1"`, "crash-create-000001"); code != 0 {
				t.Fatalf("the gateway answered %d %s instead of crashing", code, body)
			}
			if code := e.waitExit(); code != crashExit {
				t.Fatalf("exit code %d", code)
			}
			e.start(binaries.plain)
			harness = e.mustLogin(harnessAdmin, harnessPassword)
			e.resetReceivers()
			wantWrite := http.StatusForbidden
			if p.committed {
				wantWrite = http.StatusOK
			}
			if code := e.probe("bob", http.MethodPost, "/a/item"); code != wantWrite {
				t.Fatalf("after restart: bob write %d, want %d", code, wantWrite)
			}
			// The retry returns the committed receipt, or applies now.
			code, outcome, body := e.createOverride(harness, request, `"r1"`, "crash-create-000001")
			if code != http.StatusCreated || outcome.Override != 1 {
				t.Fatalf("retry: %d %s", code, body)
			}
			if n := len(e.activeOverrides(harness).Overrides); n != 1 {
				t.Fatalf("%d active overrides after the retry", n)
			}
			events := e.eventsWhen(func(events []map[string]any) bool { return countKind(events, "tpo.created") >= 1 })
			if n := countKind(events, "tpo.created"); n != 1 {
				t.Fatalf("%d creation outcome events", n)
			}
			e.stop()
			if n := queryInt(t, e.database, `SELECT COUNT(*) FROM transition WHERE kind = 'tpo.create' AND outcome = 'committed'
				AND published_at IS NOT NULL AND audited_at IS NOT NULL`); n != 1 {
				t.Fatalf("%d complete creation transitions", n)
			}
		})
	}

	ending := []point{{"tpo-revoke-before-commit", false}, {"tpo-revoke-after-commit", true}, {"tpo-revoke-after-swap", true}, {"tpo-revoke-after-receipt", true}}
	for _, p := range ending {
		t.Run("revoke/"+p.name, func(t *testing.T) {
			e := newEnv(t)
			seed(t, e.database, e.protectedPolicy(alice, bob), seedOptions{})
			e.start(binaries.plain)
			harness := e.mustLogin(harnessAdmin, harnessPassword)
			o := e.mustOverride(harness, permitOverride(overrideException, "READER", "write-a", 600), `"r1"`, "crash-revoke-create")
			e.stop()
			e.start(binaries.fault, "GATEWAY_FAULT="+p.name)
			harness = e.mustLogin(harnessAdmin, harnessPassword)
			if code, _, body := e.revokeOverride(harness, o.Override, "crash-revoke-0000001"); code != 0 {
				t.Fatalf("the gateway answered %d %s instead of crashing", code, body)
			}
			if code := e.waitExit(); code != crashExit {
				t.Fatalf("exit code %d", code)
			}
			e.start(binaries.plain)
			harness = e.mustLogin(harnessAdmin, harnessPassword)
			wantWrite := http.StatusOK
			if p.committed {
				wantWrite = http.StatusForbidden
			}
			if code := e.probe("bob", http.MethodPost, "/a/item"); code != wantWrite {
				t.Fatalf("after restart: bob write %d, want %d", code, wantWrite)
			}
			code, outcome, body := e.revokeOverride(harness, o.Override, "crash-revoke-0000001")
			if code != http.StatusOK || outcome.Disposition != overrideRevoked {
				t.Fatalf("retry: %d %s", code, body)
			}
			if e.probe("bob", http.MethodPost, "/a/item") != http.StatusForbidden {
				t.Fatal("revoked grant enforced after the retry")
			}
			events := e.eventsWhen(func(events []map[string]any) bool { return countKind(events, "tpo.revoked") >= 1 })
			if n := countKind(events, "tpo.revoked"); n != 1 {
				t.Fatalf("%d revocation outcome events", n)
			}
		})
	}

	for _, p := range []point{{"tpo-expire-before-commit", false}, {"tpo-expire-after-commit", true}, {"tpo-expire-after-swap", true}} {
		t.Run("expire/"+p.name, func(t *testing.T) {
			e := newEnv(t)
			seed(t, e.database, e.protectedPolicy(alice, bob), seedOptions{})
			e.start(binaries.fault, "GATEWAY_FAULT="+p.name)
			harness := e.mustLogin(harnessAdmin, harnessPassword)
			o := e.mustOverride(harness, permitOverride(overrideException, "READER", "write-a", 1), `"r1"`, "crash-expire-create")
			// The expiry timer ends the override and the process dies there.
			if code := e.waitExit(); code != crashExit {
				t.Fatalf("exit code %d", code)
			}
			e.start(binaries.plain)
			harness = e.mustLogin(harnessAdmin, harnessPassword)
			e.resetReceivers()
			if code := e.probe("bob", http.MethodPost, "/a/item"); code != http.StatusForbidden {
				t.Fatalf("first write after restarting past the expiry: %d", code)
			}
			e.assertReceipts(0, 0, 0)
			got, found, _ := func() (override, bool, error) {
				code, body, _ := e.as(harness, http.MethodGet, fmt.Sprintf("/admin/overrides/%d", o.Override), "")
				if code != http.StatusOK {
					return override{}, false, nil
				}
				return fromJSON[override](t, body), true, nil
			}()
			if !found || got.Status != overrideExpired || got.Resolution != fmt.Sprintf("auto-expire:%d", o.Override) || got.ResolvedAt.Before(got.ExpiresAt) {
				t.Fatalf("override after restart %+v", got)
			}
			events := e.eventsWhen(func(events []map[string]any) bool { return countKind(events, "tpo.expired") >= 1 })
			if n := countKind(events, "tpo.expired"); n != 1 {
				t.Fatalf("%d expiry outcome events", n)
			}
		})
	}
}

// A restart before an override's expiry serves it again with the same
// expiry time; a gateway stopped across the expiry ends it before serving.
// A provisional base with overrides restarts as such, and a crash during a
// rollback while an override expires is reconciled to the confirmed base
// with only the overrides still active.
func TestRestartWithOverrides(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(alice, bob), seedOptions{})
	e.start(binaries.plain)
	harness := e.mustLogin(harnessAdmin, harnessPassword)

	kept := e.mustOverride(harness, operationContainment("read-b", 600), `"r1"`, "restart-kept-000001")
	e.stop()
	e.start(binaries.plain)
	harness = e.mustLogin(harnessAdmin, harnessPassword)
	view := e.activeOverrides(harness)
	if len(view.Overrides) != 1 || !view.Overrides[0].ExpiresAt.Equal(kept.Expires) || view.Generation != kept.Generation {
		t.Fatalf("after restart %+v, created %+v", view, kept)
	}
	if e.probe("bob", http.MethodGet, "/b/x") != http.StatusForbidden {
		t.Fatal("override not enforced after restart")
	}

	short := e.mustOverride(harness, permitOverride(overrideException, "READER", "write-a", 2), `"r1"`, "restart-short-00001")
	e.stop()
	time.Sleep(time.Until(short.Expires.Add(500 * time.Millisecond)))
	// Without the deadline timer, only startup itself can have ended the
	// override before the first request.
	e.start(binaries.fault, "GATEWAY_FAULT=no-deadline-timer")
	e.resetReceivers()
	if code := e.probe("bob", http.MethodPost, "/a/item"); code != http.StatusForbidden {
		t.Fatalf("first write after restarting past the expiry: %d", code)
	}
	e.assertReceipts(0, 0, 0)
	harness = e.mustLogin(harnessAdmin, harnessPassword)
	if s := e.status(); !slices.Equal(s.Overrides, []int64{kept.Override}) {
		t.Fatalf("status %+v", s)
	}

	// A provisional base with overrides restarts as it was.
	expiring := e.mustOverride(harness, permitOverride(overrideContainment, "WRITER", "read-a", 3), `"r1"`, "restart-expiring-01")
	_, activated := e.provisionalFrom(harness, e.upstreamCandidate(), 1, 60, "restart-provisional")
	e.stop()
	e.start(binaries.plain)
	harness = e.mustLogin(harnessAdmin, harnessPassword)
	if s := e.status(); s.Provisional != activated.Provisional || s.Generation != activated.Generation {
		t.Fatalf("provisional status after restart %+v, activation %+v", s, activated)
	}
	// Crash after the rollback is committed, once the containment expired.
	e.stop()
	e.start(binaries.fault, "GATEWAY_FAULT=rollback-after-commit")
	harness = e.mustLogin(harnessAdmin, harnessPassword)
	waitFor(t, expiring.Expires.Add(5*time.Second), "the expiry", func() bool { return len(e.status().Overrides) == 1 })
	if code, _, body := e.resolve(harness, "rollback", activated.Provisional, `"r1"`, "restart-rollback-crash"); code != 0 {
		t.Fatalf("rollback answered %d %s instead of crashing", code, body)
	}
	if code := e.waitExit(); code != crashExit {
		t.Fatalf("exit code %d", code)
	}
	e.start(binaries.plain)
	harness = e.mustLogin(harnessAdmin, harnessPassword)
	s := e.status()
	if s.Confirmed != 1 || s.Provisional != 0 || !strings.HasPrefix(s.Generation, fmt.Sprintf("r1+o%d-", kept.Override)) {
		t.Fatalf("status after the crashed rollback %+v", s)
	}
	e.resetReceivers()
	if e.probe("alice", http.MethodGet, "/a/x") != http.StatusOK || e.probe("bob", http.MethodGet, "/b/x") != http.StatusForbidden {
		t.Fatal("enforcement after the crashed rollback")
	}
	e.assertReceipts(1, 0, 0)
	e.stop()
	if n := queryInt(t, e.database, `SELECT COUNT(*) FROM transition WHERE kind = 'cpa.rollback' AND outcome = 'committed' AND published_at IS NOT NULL AND audited_at IS NOT NULL`); n != 1 {
		t.Fatalf("%d reconciled rollbacks", n)
	}
}

// The test gateway moves its wall clock alone. A wall clock set back while an
// override is active does not extend it: it expires when the monotonic clock
// reaches its expiry, with no request. A wall clock set forward ends it early
// and no later request uses it. A wall clock behind the latest recorded time
// at startup keeps the gateway unavailable.
func TestOverrideExpiryUnderClockChanges(t *testing.T) {
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

	// Set back an hour after creation: still expires on time.
	setBack := e.mustOverride(harness, permitOverride(overrideException, "READER", "write-a", 2), `"r1"`, "clock-set-back-0001")
	setOffset(-time.Hour)
	waitFor(t, setBack.Expires.Add(3*time.Second), "the expiry with the wall clock set back", func() bool { return len(e.status().Overrides) == 0 })
	if e.probe("bob", http.MethodPost, "/a/item") != http.StatusForbidden {
		t.Fatal("grant used after its expiry with the wall clock set back")
	}

	// Set forward an hour: an override with ten minutes left is not used again.
	setOffset(0)
	forward := e.mustOverride(harness, permitOverride(overrideException, "READER", "write-a", 600), `"r1"`, "clock-forward-00001")
	if e.probe("bob", http.MethodPost, "/a/item") != http.StatusOK {
		t.Fatal("grant not enforced")
	}
	setOffset(time.Hour)
	e.resetReceivers()
	if code := e.probe("bob", http.MethodPost, "/a/item"); code == http.StatusOK {
		t.Fatal("grant used after the wall clock passed its expiry")
	}
	waitFor(t, time.Now().Add(5*time.Second), "the early expiry", func() bool { return len(e.status().Overrides) == 0 })
	if code := e.probe("bob", http.MethodPost, "/a/item"); code != http.StatusForbidden {
		t.Fatalf("after the early expiry: %d", code)
	}
	e.assertReceipts(0, 0, 0)

	// Created while the wall clock is an hour ahead, then set back to the
	// time the monotonic clock implies: the override still ends after its own
	// two seconds, not an hour later.
	ahead := e.mustOverride(harness, permitOverride(overrideException, "READER", "write-a", 2), `"r1"`, "clock-ahead-000001")
	setOffset(0)
	if e.probe("bob", http.MethodPost, "/a/item") != http.StatusOK {
		t.Fatal("grant not enforced before its lifetime ended")
	}
	waitFor(t, time.Now().Add(5*time.Second), "the expiry after the clock moved back", func() bool { return len(e.status().Overrides) == 0 })
	if code := e.probe("bob", http.MethodPost, "/a/item"); code != http.StatusForbidden {
		t.Fatalf("grant still used after its lifetime: %d", code)
	}
	setOffset(time.Hour)

	// Startup with the wall clock behind the latest recorded time (written an
	// hour ahead above) refuses to serve; with it restored it serves.
	e.stop()
	setOffset(0)
	e.start(binaries.fault, "GATEWAY_WALL_OFFSET="+offset)
	if health := e.health(); health != "unavailable" || e.unavailableReason() != "clock_regression" {
		t.Fatalf("health %q after the clock moved back", health)
	}
	if code := e.probe("bob", http.MethodGet, "/a/x"); code != http.StatusServiceUnavailable {
		t.Fatalf("data plane with the clock behind: %d", code)
	}
	e.stop()
	setOffset(time.Hour)
	e.start(binaries.fault, "GATEWAY_WALL_OFFSET="+offset)
	if health := e.health(); health != modeNormal {
		t.Fatalf("health %q with the clock restored", health)
	}

	e.stop()
	for _, o := range storedOverrides(t, e.database) {
		if o.Status != overrideExpired {
			t.Fatalf("override %+v", o)
		}
	}
	list := storedOverrides(t, e.database)
	// The set-back override ended within seconds of its real expiry time.
	if first := list[0]; first.ID != setBack.Override || first.ResolvedAt.Sub(first.ExpiresAt) > 2*time.Second || first.ResolvedAt.Before(first.ExpiresAt) {
		t.Fatalf("set-back override %+v ended %v after its expiry", first, first.ResolvedAt.Sub(first.ExpiresAt))
	}
	// The forward override ended about an hour early by real time, at its
	// expiry time by the wall clock it was ended by.
	if second := list[1]; second.ID != forward.Override || second.ResolvedAt.Before(second.ExpiresAt) {
		t.Fatalf("forward override %+v", second)
	}
	// The override created ahead ended within seconds of its expiry time.
	if third := list[2]; third.ID != ahead.Override || third.ResolvedAt.Before(third.ExpiresAt) || third.ResolvedAt.Sub(third.ExpiresAt) > 2*time.Second {
		t.Fatalf("override created ahead %+v ended %v after its expiry", third, third.ResolvedAt.Sub(third.ExpiresAt))
	}
}

// Stored overrides that do not fit the base they would apply to make startup
// refuse to serve rather than drop or keep them. Audit failures during an
// override change never report success that was not durable, and never keep
// serving an override whose revocation could not be completed.
func TestDamagedOverrideStateAndAuditFailures(t *testing.T) {
	damaged := []struct {
		name   string
		insert string
		reason string
	}{
		{"unknown role", `INSERT INTO override (id, type, control, operation, role, target, base, created_at, expires_at, actor, creation, status)
			VALUES (1, 'exception', 'permit', 'write-a', 'ADMIN', 'permit:ADMIN:write-a', 1, 1, ?, 'x', 'seed-override', 'active')`, "UNKNOWN_REFERENCE"},
		{"rate exception lower than the base", `INSERT INTO override (id, type, control, operation, count, target, base, created_at, expires_at, actor, creation, status)
			VALUES (1, 'exception', 'rate', 'read-a', 1, 'rate:read-a', 1, 1, ?, 'x', 'seed-override', 'active')`, "INVALID_VALUE"},
		{"unknown operation", `INSERT INTO override (id, type, control, operation, target, base, created_at, expires_at, actor, creation, status)
			VALUES (1, 'containment', 'operation', 'gone', 'operation:gone', 1, 1, ?, 'x', 'seed-override', 'active')`, "UNKNOWN_REFERENCE"},
	}
	for _, d := range damaged {
		t.Run(d.name, func(t *testing.T) {
			e := newEnv(t)
			p := e.protectedPolicy(alice, bob)
			p.Operations[0].Rate = rate(4, 60)
			seed(t, e.database, p, seedOptions{})
			db := inspect(t, e.database)
			mustExec(t, db, `INSERT INTO transition (id, kind, actor, target, digest, outcome, reason, committed_at, published_at, audited_at)
				VALUES ('seed-override', 'tpo.create', 'x', 'override', 'x', 'committed', '', 1, 1, 1)`)
			if _, err := db.Exec(d.insert, time.Now().Add(time.Hour).UnixMilli()); err != nil {
				t.Fatal(err)
			}
			db.Close()
			e.start(binaries.plain)
			if health := e.health(); health != "unavailable" {
				t.Fatalf("health %q", health)
			}
			if reason := e.unavailableReason(); !strings.Contains(reason, "overrides [1]") || !strings.Contains(reason, d.reason) {
				t.Fatalf("reason %q", reason)
			}
			e.expectNothingServed()
		})
	}

	t.Run("audit unavailable for a creation", func(t *testing.T) {
		e := newEnv(t)
		seed(t, e.database, e.protectedPolicy(alice, bob), seedOptions{})
		e.start(binaries.plain)
		harness := e.mustLogin(harnessAdmin, harnessPassword)
		e.stop()
		e.start(binaries.fault, "GATEWAY_FAULT=audit-full")
		harness = e.mustLogin(harnessAdmin, harnessPassword)
		code, outcome, body := e.createOverride(harness, permitOverride(overrideException, "READER", "write-a", 600), `"r1"`, "audit-full-create-01")
		if code != http.StatusServiceUnavailable || outcome.Reason != "AUDIT_UNAVAILABLE" {
			t.Fatalf("creation without audit: %d %s", code, body)
		}
		e.stop()
		if list := storedOverrides(t, e.database); len(list) != 0 {
			t.Fatalf("stored overrides %+v", list)
		}
	})

	t.Run("outcome not recorded for a creation", func(t *testing.T) {
		e := newEnv(t)
		seed(t, e.database, e.protectedPolicy(alice, bob), seedOptions{})
		e.start(binaries.fault, "GATEWAY_FAULT=audit-fail-published")
		harness := e.mustLogin(harnessAdmin, harnessPassword)
		code, outcome, body := e.createOverride(harness, permitOverride(overrideException, "READER", "write-a", 600), `"r1"`, "audit-pending-create")
		if code != http.StatusServiceUnavailable || outcome.Outcome != "committed" || outcome.Reason != "OUTCOME_AUDIT_PENDING" {
			t.Fatalf("creation with a lost outcome event: %d %s", code, body)
		}
		// Admission stops until a restart records the missing event.
		e.resetReceivers()
		if code := e.probe("bob", http.MethodPost, "/a/item"); code != http.StatusServiceUnavailable {
			t.Fatalf("admission with a failed audit log: %d", code)
		}
		e.assertReceipts(0, 0, 0)
		e.stop()
		e.start(binaries.plain)
		if code := e.probe("bob", http.MethodPost, "/a/item"); code != http.StatusOK {
			t.Fatalf("committed grant after restart: %d", code)
		}
		events := e.eventsWhen(func(events []map[string]any) bool { return countKind(events, "tpo.created") >= 1 })
		ev := findTransition(events, "tpo.created", outcome.Transition)
		if ev == nil || ev["reason"] != "reconciled" || ev["override"] != float64(outcome.Override) {
			t.Fatalf("reconciled outcome event %v", ev)
		}
	})

	t.Run("audit unavailable for an expiry", func(t *testing.T) {
		e := newEnv(t)
		seed(t, e.database, e.protectedPolicy(alice, bob), seedOptions{})
		e.start(binaries.plain)
		harness := e.mustLogin(harnessAdmin, harnessPassword)
		o := e.mustOverride(harness, permitOverride(overrideException, "READER", "write-a", 3), `"r1"`, "audit-expiry-create")
		// The audit file fails from now on: the expiry cannot be recorded.
		e.stop()
		e.start(binaries.fault, "GATEWAY_FAULT=audit-full")
		if health := e.health(); health != modeNormal {
			t.Fatalf("health %q before the expiry", health)
		}
		waitFor(t, o.Expires.Add(5*time.Second), "protected admission to stop", func() bool { return e.health() == "unavailable" })
		e.expectNothingServed()
		e.stop()
		if list := storedOverrides(t, e.database); len(list) != 1 || list[0].Status != overrideActive {
			t.Fatalf("an expiry that could not be recorded was committed: %+v", list)
		}
		// With the audit file working again, startup ends it before serving.
		e.start(binaries.plain)
		e.resetReceivers()
		if code := e.probe("bob", http.MethodPost, "/a/item"); code != http.StatusForbidden {
			t.Fatalf("first write after restart: %d", code)
		}
		e.assertReceipts(0, 0, 0)
		e.stop()
		if list := storedOverrides(t, e.database); list[0].Status != overrideExpired {
			t.Fatalf("override after restart %+v", list[0])
		}
	})

	t.Run("audit unavailable for a revocation", func(t *testing.T) {
		e := newEnv(t)
		seed(t, e.database, e.protectedPolicy(alice, bob), seedOptions{})
		e.start(binaries.plain)
		harness := e.mustLogin(harnessAdmin, harnessPassword)
		o := e.mustOverride(harness, permitOverride(overrideException, "READER", "write-a", 600), `"r1"`, "audit-revoke-create")
		e.stop()
		e.start(binaries.fault, "GATEWAY_FAULT=audit-full")
		harness = e.mustLogin(harnessAdmin, harnessPassword)
		code, outcome, body := e.revokeOverride(harness, o.Override, "audit-full-revoke-01")
		if code != http.StatusServiceUnavailable || outcome.Reason != "AUDIT_UNAVAILABLE" {
			t.Fatalf("revocation without audit: %d %s", code, body)
		}
		// The grant being withdrawn is not served any more.
		if health := e.health(); health != "unavailable" {
			t.Fatalf("health %q", health)
		}
		e.expectNothingServed()
		// After a restart nothing was revoked: the override is still active
		// and can be revoked properly.
		e.stop()
		e.start(binaries.plain)
		harness = e.mustLogin(harnessAdmin, harnessPassword)
		if code, outcome, body := e.revokeOverride(harness, o.Override, "audit-revoke-retry-01"); code != http.StatusOK || outcome.Disposition != overrideRevoked {
			t.Fatalf("revocation after restart: %d %s", code, body)
		}
	})
}

// A declared historical restore of a backup taken while an override was
// active never brings that override back, even though it was revoked after
// the backup and its stored expiry is still in the future: the restored
// database is marked recovery_required before it becomes live, every
// restart keeps it so, and recovery ends under a deny-all base with no
// override. An ordinary restart of the current database keeps the override.
func TestHistoricalRestoreQuarantinesOverrides(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(alice, bob), seedOptions{})
	e.start(binaries.plain)
	harness := e.mustLogin(harnessAdmin, harnessPassword)
	grant := e.mustOverride(harness, permitOverride(overrideException, "READER", "write-a", 3600), `"r1"`, "restore-grant-00001")

	// Ordinary restart: the current database keeps the eligible override.
	e.stop()
	e.start(binaries.plain)
	if code := e.probe("bob", http.MethodPost, "/a/item"); code != http.StatusOK {
		t.Fatalf("grant after an ordinary restart: %d", code)
	}

	backup := filepath.Join(t.TempDir(), "with-override.db")
	e.stop()
	e.backup(backup)
	e.start(binaries.plain)
	harness = e.mustLogin(harnessAdmin, harnessPassword)
	if code, _, body := e.revokeOverride(harness, grant.Override, "restore-revoke-0001"); code != http.StatusOK {
		t.Fatalf("revocation: %d %s", code, body)
	}
	e.stop()

	// An interrupted restore leaves the current database, which serves the
	// revocation.
	if out, code := e.runOperator(binaries.fault, []string{"GATEWAY_FAULT=restore-before-promote"}, "restore", "--backup", backup); code != crashExit {
		t.Fatalf("interrupted restore exited %d: %s", code, out)
	}
	e.start(binaries.plain)
	if code := e.probe("bob", http.MethodPost, "/a/item"); code != http.StatusForbidden {
		t.Fatalf("after an interrupted restore: %d", code)
	}
	e.stop()

	if out, code := e.runOperator(binaries.plain, nil, "restore", "--backup", backup); code != 0 {
		t.Fatalf("restore exited %d: %s", code, out)
	}
	if mode := queryString(t, e.database, `SELECT mode FROM deployment`); mode != modeRecovery {
		t.Fatalf("restored mode %q", mode)
	}
	list := storedOverrides(t, e.database)
	if len(list) != 1 || list[0].Status != overrideQuarantined || list[0].ResolvedBy != "operator" || !list[0].ExpiresAt.After(time.Now()) {
		t.Fatalf("restored overrides %+v", list)
	}
	for range 2 {
		e.start(binaries.plain)
		if health := e.health(); health != modeRecovery {
			t.Fatalf("health %q after the restore", health)
		}
		e.expectNothingServed()
		e.stop()
	}

	root := e.recoverAdministrator()
	e.establishRecoveryBase(root)
	view := e.activeOverrides(root)
	if len(view.Overrides) != 0 || len(view.GenerationOverrides) != 0 {
		t.Fatalf("overrides after recovery %+v", view)
	}
	e.expectNothingServed()
	if code, outcome, body := e.revokeOverride(root, grant.Override, "restore-revoke-0002"); code != http.StatusConflict || outcome.Disposition != overrideQuarantined {
		t.Fatalf("revocation of the quarantined override: %d %s", code, body)
	}
	e.stop()
	if list := storedOverrides(t, e.database); list[0].Status != overrideQuarantined {
		t.Fatalf("override after recovery %+v", list[0])
	}
}

// A declared historical restore of a backup holding several active overrides
// on distinct targets, with or without a provisional base, quarantines all of
// them under the one restore transition. Each keeps its own identity, target
// and creation history, and none is served again, although one was revoked
// after the backup and every stored expiry is still in the future. An
// interrupted restore leaves the live database authoritative and unchanged;
// repeating the restore completes it. Recovery ends under a deny-all base with
// no override, and a quarantined override can never be revoked or resumed.
func TestHistoricalRestoreQuarantinesSeveralOverrides(t *testing.T) {
	for _, withProvisional := range []bool{false, true} {
		name := "overrides"
		if withProvisional {
			name = "overrides and a provisional base"
		}
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			seed(t, e.database, e.protectedPolicy(alice, bob), seedOptions{})
			e.start(binaries.plain)
			harness := e.mustLogin(harnessAdmin, harnessPassword)
			grant := e.mustOverride(harness, permitOverride(overrideException, "READER", "write-a", 3600), `"r1"`, "several-grant-00001")
			e.mustOverride(harness, operationContainment("read-b", 3600), `"r1"`, "several-contain-0001")
			if withProvisional {
				e.provisionalFrom(harness, e.upstreamCandidate(), 1, 3600, "several-provisional")
			}
			if s := e.status(); len(s.Overrides) != 2 || (s.Provisional == 2) != withProvisional {
				t.Fatalf("status before the backup %+v", s)
			}
			e.stop()
			backup := filepath.Join(t.TempDir(), "several-overrides.db")
			e.backup(backup)
			backedUp := storedOverrides(t, backup)

			e.start(binaries.plain)
			harness = e.mustLogin(harnessAdmin, harnessPassword)
			if code, _, body := e.revokeOverride(harness, grant.Override, "several-revoke-0001"); code != http.StatusOK {
				t.Fatalf("revocation after the backup: %d %s", code, body)
			}
			e.stop()

			// An interrupted restore changes nothing live; the staged copy was
			// already marked before it could have been promoted. Reading the
			// stopped database first applies its write-ahead log, so the file
			// hash compared below is the whole live state.
			if mode := queryString(t, e.database, `SELECT mode FROM deployment`); mode != modeNormal {
				t.Fatalf("live mode %q", mode)
			}
			live := fileSHA256(t, e.database)
			if out, code := e.runOperator(binaries.fault, []string{"GATEWAY_FAULT=restore-before-promote"}, "restore", "--backup", backup); code != crashExit {
				t.Fatalf("interrupted restore exited %d: %s", code, out)
			}
			if fileSHA256(t, e.database) != live {
				t.Fatal("the interrupted restore changed the live database")
			}
			if retired, _ := filepath.Glob(e.database + ".replaced-*"); len(retired) != 0 {
				t.Fatalf("the interrupted restore retired the live database: %v", retired)
			}
			if mode := queryString(t, e.database+".restore", `SELECT mode FROM deployment`); mode != modeRecovery {
				t.Fatalf("staged copy mode %q", mode)
			}
			e.start(binaries.plain)
			if health := e.health(); health != modeNormal {
				t.Fatalf("health %q after the interrupted restore", health)
			}
			if a, b := e.probe("bob", http.MethodPost, "/a/item"), e.probe("bob", http.MethodGet, "/b/x"); a != http.StatusForbidden || b != http.StatusForbidden {
				t.Fatalf("revoked grant and active containment after the interrupted restore: %d %d", a, b)
			}
			if code := e.probe("alice", http.MethodGet, "/a/x"); code != http.StatusOK {
				t.Fatalf("live base after the interrupted restore: %d", code)
			}
			e.stop()

			if mode := queryString(t, e.database, `SELECT mode FROM deployment`); mode != modeNormal {
				t.Fatalf("live mode %q before the restore", mode)
			}
			live = fileSHA256(t, e.database)
			if out, code := e.runOperator(binaries.plain, nil, "restore", "--backup", backup); code != 0 || !strings.Contains(out, modeRecovery) {
				t.Fatalf("restore exited %d: %s", code, out)
			}
			if retired, _ := filepath.Glob(e.database + ".replaced-*"); len(retired) != 1 || fileSHA256(t, retired[0]) != live {
				t.Fatalf("the replaced live database is not kept unchanged: %v", retired)
			}

			// Every override active in the backup is quarantined, changed in
			// nothing but its disposition, and all name one restore transition.
			restored := storedOverrides(t, e.database)
			if len(restored) != 2 {
				t.Fatalf("restored overrides %+v", restored)
			}
			resolution := restored[0].Resolution
			for i, o := range restored {
				unchanged := o
				unchanged.Status, unchanged.ResolvedAt, unchanged.ResolvedBy, unchanged.Resolution = overrideActive, nil, "", ""
				if unchanged != backedUp[i] || o.Status != overrideQuarantined || o.ResolvedBy != "operator" || o.ResolvedAt == nil ||
					o.Resolution != resolution || !o.ExpiresAt.After(time.Now()) {
					t.Fatalf("override restored as %+v from %+v", o, backedUp[i])
				}
			}
			if resolution != "operator:restore:"+queryString(t, e.database, `SELECT recovery_episode FROM deployment`) {
				t.Fatalf("resolution %q is not the restore transition of the recovery episode", resolution)
			}
			quarantinedActivations := 0
			if withProvisional {
				quarantinedActivations = 1
			}
			for query, want := range map[string]int{
				`SELECT COUNT(*) FROM transition WHERE kind = 'operator.restore'`:                                         1,
				`SELECT COUNT(*) FROM transition WHERE id = '` + resolution + `' AND outcome = 'committed'`:               1,
				`SELECT COUNT(*) FROM activation WHERE disposition = 'quarantined' AND resolution = '` + resolution + `'`: quarantinedActivations,
				`SELECT COUNT(*) FROM base WHERE confirmed = 1 AND provisional IS NULL AND deadline IS NULL`:              1,
				`SELECT COUNT(*) FROM override WHERE status = 'active'`:                                                   0,
				`SELECT COUNT(*) FROM pragma_foreign_key_check`:                                                           0,
			} {
				if n := queryInt(t, e.database, query); n != want {
					t.Errorf("%s = %d, want %d", query, n, want)
				}
			}
			if check := queryString(t, e.database, `PRAGMA integrity_check`); check != "ok" {
				t.Fatalf("integrity check of the restored database %q", check)
			}

			for range 2 {
				e.start(binaries.plain)
				if health := e.health(); health != modeRecovery {
					t.Fatalf("health %q after the restore", health)
				}
				e.expectNothingServed()
				e.stop()
			}

			// Only re-established trust and a deny-all recovery base end the
			// recovery; restored administrators stay quarantined and no
			// override can be created or revoked meanwhile.
			root := e.recoverAdministrator()
			if _, code := e.login(harnessAdmin, harnessPassword); code != http.StatusUnauthorized {
				t.Fatalf("restored administrator during recovery: %d", code)
			}
			if code, outcome, body := e.createOverride(root, operationContainment("read-a", 60), `"r1"`, "several-recovery-create"); code != http.StatusConflict || outcome.Reason != "INVALID_STATE" {
				t.Fatalf("override creation during recovery: %d %s", code, body)
			}
			if code, outcome, body := e.revokeOverride(root, grant.Override, "several-recovery-revoke"); code != http.StatusConflict || outcome.Reason != "INVALID_STATE" {
				t.Fatalf("revocation during recovery: %d %s", code, body)
			}
			e.expectNothingServed()
			recovery := e.establishRecoveryBase(root)
			code, body, _ := e.as(root, http.MethodGet, "/admin/status", "")
			if s := fromJSON[status](t, body); code != http.StatusOK || s.State != modeNormal || s.Confirmed != recovery.Revision || s.Provisional != 0 || len(s.Overrides) != 0 {
				t.Fatalf("status after the recovery base: %d %s", code, body)
			}
			if view := e.activeOverrides(root); len(view.Overrides) != 0 || len(view.GenerationOverrides) != 0 {
				t.Fatalf("overrides after recovery %+v", view)
			}
			e.expectNothingServed()
			for i, o := range restored {
				code, outcome, body := e.revokeOverride(root, o.ID, fmt.Sprintf("several-after-revoke-%d", i))
				if code != http.StatusConflict || outcome.Reason != "ALREADY_RESOLVED" || outcome.Disposition != overrideQuarantined {
					t.Fatalf("revocation of quarantined override %d: %d %s", o.ID, code, body)
				}
			}
			if withProvisional {
				code, outcome, body := e.resolve(root, "confirm", 2, policyETag(recovery.Revision), "several-confirm-0001")
				if code != http.StatusConflict || outcome.Reason != "ALREADY_RESOLVED" || outcome.Disposition != dispositionQuarantined {
					t.Fatalf("confirmation of the quarantined provisional base: %d %s", code, body)
				}
			}
			restores := 0
			for _, ev := range e.auditEvents() {
				id, _ := ev["transition"].(string)
				if ev["kind"] == "transition.committed" && strings.HasPrefix(id, "operator:restore:") {
					restores++
				}
			}
			if restores != 1 {
				t.Fatalf("%d committed restore events, want 1 (the interrupted attempt has only its intent)", restores)
			}
			e.stop()
			if after := storedOverrides(t, e.database); fmt.Sprintf("%+v", after) != fmt.Sprintf("%+v", restored) {
				t.Fatalf("quarantined overrides changed during recovery: %+v", after)
			}
		})
	}
}

// A schema version 2 database, including one with a provisional base, is
// served only after the migrate command; afterwards overrides can be used.
func TestMigrateFromVersionTwo(t *testing.T) {
	e := newEnv(t)
	candidate := e.upstreamCandidate()
	seed(t, e.database, e.protectedPolicy(alice, bob), seedOptions{version: 2, provisional: &candidate, deadline: time.Now().Add(time.Hour)})
	e.start(binaries.plain)
	if reason := e.unavailableReason(); !strings.Contains(reason, "schema version 2") || !strings.Contains(reason, "migrate") {
		t.Fatalf("reason %q", reason)
	}
	e.stop()
	if out, code := e.runOperator(binaries.plain, nil, "migrate"); code != 0 || !strings.Contains(out, "from schema version 2 to 4") {
		t.Fatalf("migrate exited %d: %s", code, out)
	}
	e.start(binaries.plain)
	harness := e.mustLogin(harnessAdmin, harnessPassword)
	if s := e.status(); s.Provisional != 2 || s.Confirmed != 1 {
		t.Fatalf("status after migration %+v", s)
	}
	if code, outcome, body := e.createOverride(harness, operationContainment("read-b", 60), `"r1"`, "migrated-create-01"); code != http.StatusConflict || outcome.Reason != "PROVISIONAL_ACTIVE" {
		t.Fatalf("creation while provisional: %d %s", code, body)
	}
	if code, _, body := e.resolve(harness, "rollback", 2, `"r1"`, "migrated-rollback-01"); code != http.StatusOK {
		t.Fatalf("rollback: %d %s", code, body)
	}
	e.mustOverride(harness, operationContainment("read-b", 60), `"r1"`, "migrated-create-02")
	if e.probe("bob", http.MethodGet, "/b/x") != http.StatusForbidden {
		t.Fatal("override after migration not enforced")
	}
}

// version3Grant is the request that created the active override written by
// writeVersion3Overrides.
var version3Grant = permitOverride(overrideException, "READER", "write-a", 3600)

// writeVersion3Overrides adds to a seeded database the override history a
// schema version 3 gateway records: override 1 revoked, 2 expired, 3
// quarantined by an earlier restore and 4 active, each with its committed,
// published and audited transitions. The revocation and the active override's
// creation carry the digests of their Admin API requests, so they can be
// replayed.
func writeVersion3Overrides(t *testing.T, path string) {
	t.Helper()
	now := time.Now()
	ms := func(d time.Duration) int64 { return now.Add(d).UnixMilli() }
	db := inspect(t, path)
	transition := `INSERT INTO transition (id, kind, actor, target, expected, digest, outcome, reason, committed_at, published_at, published_by, audited_at)
		VALUES (?, ?, ?, ?, ?, ?, 'committed', '', ?, ?, 'publisher', ?)`
	transitions := []struct {
		id, kind, actor, target string
		expected                any
		digest                  string
		at                      int64
	}{
		{"admin:harness:version3-grant-00001", "tpo.create", harnessAdmin, "override", 1, "version3-grant", ms(-2 * time.Hour)},
		{"admin:harness:version3-revoke-0001", "tpo.revoke", harnessAdmin, "override 1", nil, requestDigest("tpo.revoke", "override 1", sql.NullInt64{}, nil), ms(-119 * time.Minute)},
		{"admin:harness:version3-close-00001", "tpo.create", harnessAdmin, "override", 1, "version3-close", ms(-2 * time.Hour)},
		{"auto-expire:2", "tpo.expire", "controller", "override 2", nil, "auto-expire:2", ms(-119 * time.Minute)},
		{"admin:harness:version3-limit-00001", "tpo.create", harnessAdmin, "override", 1, "version3-limit", ms(-2 * time.Hour)},
		{"operator:restore:" + strings.Repeat("e", 32), "operator.restore", "operator", "older.db", nil, "version3-restore", ms(-118 * time.Minute)},
		{"admin:harness:version3-grant-00002", "tpo.create", harnessAdmin, "override", 1,
			requestDigest("tpo.create", "override", sql.NullInt64{Int64: 1, Valid: true}, version3Grant), ms(-time.Minute)},
	}
	for _, tr := range transitions {
		if _, err := db.Exec(transition, tr.id, tr.kind, tr.actor, tr.target, tr.expected, tr.digest, tr.at, tr.at, tr.at); err != nil {
			t.Fatalf("transition %s: %v", tr.id, err)
		}
	}
	override := `INSERT INTO override (id, type, control, operation, role, target, base, created_at, expires_at, actor, creation, status, resolved_at, resolved_by, resolution)
		VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?, 'harness', ?, ?, ?, ?, ?)`
	overrides := [][]any{
		{1, overrideException, controlPermit, "write-a", "READER", "permit:READER:write-a", ms(-2 * time.Hour), ms(-110 * time.Minute),
			"admin:harness:version3-grant-00001", overrideRevoked, ms(-119 * time.Minute), harnessAdmin, "admin:harness:version3-revoke-0001"},
		{2, overrideContainment, controlOperation, "read-b", nil, "operation:read-b", ms(-2 * time.Hour), ms(-119 * time.Minute),
			"admin:harness:version3-close-00001", overrideExpired, ms(-119 * time.Minute), "controller", "auto-expire:2"},
		{3, overrideContainment, controlPermit, "read-a", "READER", "permit:READER:read-a", ms(-2 * time.Hour), ms(time.Hour),
			"admin:harness:version3-limit-00001", overrideQuarantined, ms(-118 * time.Minute), "operator", "operator:restore:" + strings.Repeat("e", 32)},
		{4, overrideException, controlPermit, "write-a", "READER", "permit:READER:write-a", ms(-time.Minute), ms(59 * time.Minute),
			"admin:harness:version3-grant-00002", overrideActive, nil, nil, nil},
	}
	for _, o := range overrides {
		if _, err := db.Exec(override, o...); err != nil {
			t.Fatalf("override %v: %v", o[0], err)
		}
	}
	db.Close()
}

// A schema version 3 database, with overrides in every state, is served only
// after the migrate command. The migration keeps every override and every
// earlier record exactly as stored and produces exactly the schema an
// installation creates; a database held by another process, an interrupted
// migration and damaged or newer databases are left unchanged. Afterwards
// transitions recorded before the migration replay to their receipts, and the
// active override is still enforced and can be revoked.
func TestMigrateFromVersionThree(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(alice, bob), seedOptions{version: 3})
	writeVersion3Overrides(t, e.database)
	before := map[string][]string{}
	for _, table := range []string{"override", "revision", "base", "candidate", "administrator", "enrolment", "activation", "transition"} {
		before[table] = queryRows(t, e.database, `SELECT * FROM `+table+` ORDER BY rowid`)
	}
	expectVersion3 := func(stage string) {
		t.Helper()
		db := inspect(t, e.database)
		if err := checkDatabaseSchema(db, 3); err != nil || count(t, db, `PRAGMA user_version`) != 3 {
			t.Fatalf("%s: not the version 3 schema: %v", stage, err)
		}
		db.Close()
		for table, rows := range before {
			if after := queryRows(t, e.database, `SELECT * FROM `+table+` ORDER BY rowid`); !slices.Equal(after, rows) {
				t.Fatalf("%s: table %s changed", stage, table)
			}
		}
	}

	e.start(binaries.plain)
	if reason := e.unavailableReason(); !strings.Contains(reason, "schema version 3") || !strings.Contains(reason, "migrate") {
		t.Fatalf("reason %q", reason)
	}
	e.stop()

	// The database is held as a running gateway holds its own.
	held, err := connect(e.database, "WAL")
	if err != nil {
		t.Fatal(err)
	}
	if out, code := e.runOperator(binaries.plain, nil, "migrate"); code == 0 || !strings.Contains(out, "held by another gateway process") {
		t.Fatalf("migrate of a held database: %d %s", code, out)
	}
	held.Close()
	expectVersion3("after a refused migration")
	if out, code := e.runOperator(binaries.fault, []string{"GATEWAY_FAULT=migrate-before-commit"}, "migrate"); code != crashExit {
		t.Fatalf("interrupted migrate exited %d: %s", code, out)
	}
	expectVersion3("after an interrupted migration")

	if out, code := e.runOperator(binaries.plain, nil, "migrate"); code != 0 || !strings.Contains(out, "from schema version 3 to 4") {
		t.Fatalf("migrate exited %d: %s", code, out)
	}
	if out, code := e.runOperator(binaries.plain, nil, "migrate"); code == 0 || !strings.Contains(out, "cannot be migrated") {
		t.Fatalf("second migrate: %d %s", code, out)
	}
	installed := newEnv(t)
	if out, code := installed.runOperator(binaries.plain, nil, "install", "--admin", "root"); code != 0 {
		t.Fatalf("install exited %d: %s", code, out)
	}
	schema := `SELECT type, name, tbl_name, sql FROM sqlite_schema ORDER BY type, name`
	if migrated, fresh := queryRows(t, e.database, schema), queryRows(t, installed.database, schema); !slices.Equal(migrated, fresh) {
		t.Fatalf("migrated schema differs from an installed one:\n%v\n%v", migrated, fresh)
	}
	for table, rows := range before {
		after := queryRows(t, e.database, `SELECT * FROM `+table+` ORDER BY rowid`)
		if table == "transition" {
			// The migration adds its own record and nothing else.
			if len(after) != len(rows)+1 || !slices.Equal(after[:len(rows)], rows) {
				t.Fatalf("transitions after the migration:\n%v", after)
			}
			continue
		}
		if !slices.Equal(after, rows) {
			t.Fatalf("table %s changed by the migration:\n%v\n%v", table, rows, after)
		}
	}
	for query, want := range map[string]int{
		`PRAGMA user_version`: 4,
		`SELECT COUNT(*) FROM transition WHERE kind = 'operator.migrate' AND outcome = 'committed' AND audited_at IS NOT NULL`: 1,
		`SELECT COUNT(*) FROM pragma_foreign_key_check`: 0,
	} {
		if n := queryInt(t, e.database, query); n != want {
			t.Fatalf("%s = %d, want %d", query, n, want)
		}
	}
	if check := queryString(t, e.database, `PRAGMA integrity_check`); check != "ok" {
		t.Fatalf("integrity check %q", check)
	}

	e.start(binaries.plain)
	harness := e.mustLogin(harnessAdmin, harnessPassword)
	if s := e.status(); s.State != modeNormal || !slices.Equal(s.Overrides, []int64{4}) {
		t.Fatalf("status after migration %+v", s)
	}
	code, replayed, body := e.revokeOverride(harness, 1, "version3-revoke-0001")
	if code != http.StatusOK || replayed.Transition != "admin:harness:version3-revoke-0001" || replayed.Override != 1 || replayed.Disposition != overrideRevoked {
		t.Fatalf("replayed revocation: %d %s", code, body)
	}
	code, replayed, body = e.createOverride(harness, version3Grant, `"r1"`, "version3-grant-00002")
	if code != http.StatusCreated || replayed.Override != 4 || replayed.Disposition != "" {
		t.Fatalf("replayed creation: %d %s", code, body)
	}
	if code := e.probe("bob", http.MethodPost, "/a/item"); code != http.StatusOK {
		t.Fatalf("active override after migration: %d", code)
	}
	if code, ended, body := e.revokeOverride(harness, 4, "version4-revoke-0001"); code != http.StatusOK || ended.Disposition != overrideRevoked {
		t.Fatalf("revocation after migration: %d %s", code, body)
	}
	if code := e.probe("bob", http.MethodPost, "/a/item"); code != http.StatusForbidden {
		t.Fatalf("revoked override after migration: %d", code)
	}
	e.stop()
	if list := storedOverrides(t, e.database); len(list) != 4 || list[3].Status != overrideRevoked || list[3].Resolution != "admin:harness:version4-revoke-0001" {
		t.Fatalf("overrides after the revocation %+v", list)
	}

	// Damaged or newer databases are refused unchanged. Damage is written as
	// something other than the gateway would write it: with foreign key or
	// CHECK enforcement off, and any guard in the way removed and put back
	// unchanged.
	withoutGuards := func(db *sql.DB, triggers []string, statements ...string) {
		saved := make([]string, len(triggers))
		for i, name := range triggers {
			if err := db.QueryRow(`SELECT sql FROM sqlite_schema WHERE name = ?`, name).Scan(&saved[i]); err != nil {
				t.Fatal(err)
			}
			mustExec(t, db, `DROP TRIGGER `+name)
		}
		mustExec(t, db, statements...)
		mustExec(t, db, saved...)
	}
	refusals := []struct {
		name    string
		version int
		damage  func(db *sql.DB)
		reason  string
	}{
		{"newer schema version", databaseVersion, func(db *sql.DB) {
			mustExec(t, db, `PRAGMA user_version = 5`)
		}, "schema version 5 cannot be migrated"},
		{"override naming a missing revision", 3, func(db *sql.DB) {
			withoutGuards(db, []string{"override_fixed"}, `PRAGMA foreign_keys = OFF`, `UPDATE override SET base = 99 WHERE id = 4`)
		}, "FOREIGN KEY constraint failed"},
		{"override outside its constraints", 3, func(db *sql.DB) {
			withoutGuards(db, []string{"override_final"}, `PRAGMA ignore_check_constraints = ON`, `UPDATE override SET resolved_by = NULL WHERE id = 1`)
		}, "CHECK constraint failed"},
		{"transition naming a missing revision", 3, func(db *sql.DB) {
			mustExec(t, db, `PRAGMA foreign_keys = OFF`, `UPDATE transition SET result_revision = 99 WHERE id = 'auto-expire:2'`)
		}, "foreign key check failed"},
		{"administrator outside its constraints", 3, func(db *sql.DB) {
			mustExec(t, db, `PRAGMA ignore_check_constraints = ON`, `UPDATE administrator SET permissions = 16`)
		}, "integrity check failed"},
	}
	for _, r := range refusals {
		other := newEnv(t)
		seed(t, other.database, other.protectedPolicy(alice, bob), seedOptions{version: r.version})
		if r.version == 3 {
			writeVersion3Overrides(t, other.database)
		}
		db, err := sql.Open("sqlite", "file:"+other.database)
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		r.damage(db)
		db.Close()
		damaged := fileSHA256(t, other.database)
		if out, code := other.runOperator(binaries.plain, nil, "migrate"); code == 0 || !strings.Contains(out, r.reason) {
			t.Errorf("%s: migrate exited %d: %s", r.name, code, out)
		}
		if fileSHA256(t, other.database) != damaged {
			t.Errorf("%s: a refused migration changed the database", r.name)
		}
	}
	damaged := newEnv(t)
	seed(t, damaged.database, damaged.protectedPolicy(alice, bob), seedOptions{version: 3})
	corrupt(t, damaged.database)
	corrupted := fileSHA256(t, damaged.database)
	if out, code := damaged.runOperator(binaries.plain, nil, "migrate"); code == 0 || fileSHA256(t, damaged.database) != corrupted {
		t.Errorf("migrate of a corrupted file exited %d and changed it: %s", code, out)
	}
}

// Overrides through the three fixture APIs, whose own receipts and state
// show what was executed, then the reset to the stable state for
// subsequent requests: one confirmed base, no provisional base, no active
// override, empty rate counters and fixtures at their seeds.
func TestOverridesWithFixtures(t *testing.T) {
	e, fixtures := newFixtureEnv(t)
	harness := e.mustLogin(harnessAdmin, harnessPassword)
	initial := e.status()
	seedHash := map[string]string{}
	for _, f := range loadCatalogue(t).Fixtures {
		seedHash[f.Service] = f.SeedSHA256
	}
	resetFixtures := func() {
		for _, f := range fixtures {
			f.reset()
		}
	}

	// Exception: staff may read the sales report.
	resetFixtures()
	if code, _ := e.get("/reports/sales", bearer(t, "staff-01")...); code != http.StatusForbidden || receiptCounts(fixtures)["reports"] != 0 {
		t.Fatalf("sales report under the base: %d", code)
	}
	sales := e.mustOverride(harness, permitOverride(overrideException, "STAFF", "reports.sales", 600), `"r1"`, "fixtures-sales-00001")
	if code, _ := e.get("/reports/sales", bearer(t, "staff-01")...); code != http.StatusOK || receiptCounts(fixtures)["reports"] != 1 {
		t.Fatalf("sales report under the exception: %d %v", code, receiptCounts(fixtures))
	}
	// Still a bearer token is required.
	if code, _ := e.get("/reports/sales"); code != http.StatusUnauthorized || receiptCounts(fixtures)["reports"] != 1 {
		t.Fatalf("anonymous sales report: %d", code)
	}

	// Containment: no orders may be created.
	order := `{"customer":"cust-001","item":"ITM-001","quantity":1}`
	block := e.mustOverride(harness, operationContainment("orders.create", 600), `"r1"`, "fixtures-block-00001")
	resetFixtures()
	if code, _ := e.request(http.MethodPost, "/orders", "application/json", order, bearer(t, "cust-001")...); code != http.StatusForbidden ||
		receiptCounts(fixtures)["orders"] != 0 || fixtures["orders"].state().SHA256 != seedHash["orders"] {
		t.Fatalf("order creation under the containment: %d", code)
	}
	// Rate containment: staff may list orders once per window.
	limit := e.mustOverride(harness, rateOverride(overrideContainment, "orders.list", 1, 600), `"r1"`, "fixtures-limit-00001")
	codes := []int{}
	for range 2 {
		code, _ := e.get("/orders", bearer(t, "staff-01")...)
		codes = append(codes, code)
	}
	if !slices.Equal(codes, []int{200, 429}) || receiptCounts(fixtures)["orders"] != 1 {
		t.Fatalf("order lists under the rate containment: %v %v", codes, receiptCounts(fixtures))
	}
	// Ending the containment restores creation, still schema-validated.
	if code, _, body := e.revokeOverride(harness, block.Override, "fixtures-block-revoke"); code != http.StatusOK {
		t.Fatalf("revocation: %d %s", code, body)
	}
	resetFixtures()
	if code, _ := e.request(http.MethodPost, "/orders", "application/json", `{"customer":"cust-001"}`, bearer(t, "cust-001")...); code != http.StatusBadRequest ||
		receiptCounts(fixtures)["orders"] != 0 {
		t.Fatalf("invalid order: %d", code)
	}
	if code, _ := e.request(http.MethodPost, "/orders", "application/json", order, bearer(t, "cust-001")...); code != http.StatusCreated ||
		receiptCounts(fixtures)["orders"] != 1 || fixtures["orders"].state().SHA256 == seedHash["orders"] {
		t.Fatalf("order creation after the containment: %d", code)
	}

	// Reset: end every override, then restart (rate counters are process
	// memory) and reset the fixtures.
	for i, id := range []int64{sales.Override, limit.Override} {
		if code, _, body := e.revokeOverride(harness, id, fmt.Sprintf("fixtures-reset-%02d-xx", i)); code != http.StatusOK {
			t.Fatalf("revocation: %d %s", code, body)
		}
	}
	e.stop()
	e.start(binaries.plain)
	harness = e.mustLogin(harnessAdmin, harnessPassword)
	resetFixtures()
	s := e.status()
	view := e.activeOverrides(harness)
	if s.Provisional != 0 || s.Confirmed != initial.Confirmed || s.Generation != initial.Generation || len(s.Overrides) != 0 || len(view.Overrides) != 0 {
		t.Fatalf("state after the reset %+v, %+v; initial %+v", s, view, initial)
	}
	if code, _, _ := e.as(harness, http.MethodGet, "/admin/provisional", ""); code != http.StatusNotFound {
		t.Fatalf("provisional view after the reset: %d", code)
	}
	for name, f := range fixtures {
		if state := f.state(); state.SHA256 != seedHash[name] || len(f.receipts()) != 0 {
			t.Fatalf("fixture %s after the reset: %s", name, state.SHA256)
		}
	}
	// The counter the containment used is empty again: staff list orders.
	if code, _ := e.get("/orders", bearer(t, "staff-01")...); code != http.StatusOK {
		t.Fatalf("order list after the reset: %d", code)
	}
	fixtures["orders"].reset()
}

// Under continuous traffic, overrides are created and revoked repeatedly:
// every request is decided and forwarded by one generation, and a write only
// the grant permits reaches the upstream only under a generation that has
// the grant.
func TestOverridesUnderLoad(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(alice, bob), seedOptions{})
	e.start(binaries.plain)
	harness := e.mustLogin(harnessAdmin, harnessPassword)
	withGrant := map[string]bool{}
	withoutGrant := map[string]bool{e.status().Generation: true}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var unexpected sync.Map
	var forwarded atomic.Int64
	bobToken := "Bearer " + issuer.tokenFor(t, "bob")
	aliceToken := "Bearer " + issuer.tokenFor(t, "alice")
	for i := range 6 {
		write := i%2 == 0
		wg.Go(func() {
			client := &http.Client{Timeout: 10 * time.Second}
			for {
				select {
				case <-stop:
					return
				default:
				}
				req, _ := http.NewRequest(http.MethodGet, "http://"+e.dataAddr+"/a/x", nil)
				req.Header.Set("Authorization", aliceToken)
				if write {
					req, _ = http.NewRequest(http.MethodPost, "http://"+e.dataAddr+"/a/item", strings.NewReader(`{"n":1}`))
					req.Header.Set("Authorization", bobToken)
					req.Header.Set("Content-Type", "application/json")
				}
				resp, err := client.Do(req)
				if err != nil {
					unexpected.Store(err.Error(), true)
					continue
				}
				resp.Body.Close()
				switch {
				case resp.StatusCode == http.StatusOK:
					forwarded.Add(1)
				case resp.StatusCode == http.StatusForbidden && write:
				default:
					unexpected.Store(fmt.Sprintf("write %v: %d", write, resp.StatusCode), true)
				}
			}
		})
	}
	for round := range 6 {
		time.Sleep(100 * time.Millisecond)
		created := e.mustOverride(harness, permitOverride(overrideException, "READER", "write-a", 600), `"r1"`, fmt.Sprintf("load-create-%02d-xxxx", round))
		withGrant[created.Generation] = true
		time.Sleep(100 * time.Millisecond)
		code, revoked, body := e.revokeOverride(harness, created.Override, fmt.Sprintf("load-revoke-%02d-xxxx", round))
		if code != http.StatusOK {
			t.Fatalf("revocation: %d %s", code, body)
		}
		withoutGrant[revoked.Generation] = true
	}
	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()
	unexpected.Range(func(key, _ any) bool {
		t.Errorf("unexpected answer: %v", key)
		return true
	})
	writes, reads := 0, 0
	for _, r := range e.a.receipts() {
		switch {
		case r.Method == http.MethodPost && withGrant[r.Generation] && r.Headers.Get("X-Gateway-Subject") == "bob":
			writes++
		case r.Method == http.MethodGet && (withGrant[r.Generation] || withoutGrant[r.Generation]):
			reads++
		default:
			t.Fatalf("A received %s by %s under generation %s", r.Method, r.Headers.Get("X-Gateway-Subject"), r.Generation)
		}
	}
	if writes == 0 || reads == 0 {
		t.Fatalf("traffic did not exercise both states: %d writes, %d reads", writes, reads)
	}
	t.Logf("forwarded %d: %d granted writes, %d reads", forwarded.Load(), writes, reads)
}

// With overrides active, routing and forwarding are unchanged: encoded,
// dot-segment and case variants are refused before routing, an unknown
// method is not allowed, a contained operation reaches nothing, and a failed
// upstream is reported without any other destination being tried.
func TestOverridesKeepRoutingAndUpstreams(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(alice, bob), seedOptions{})
	e.start(binaries.plain)
	harness := e.mustLogin(harnessAdmin, harnessPassword)
	e.mustOverride(harness, operationContainment("read-b", 600), `"r1"`, "routing-close-00001")
	e.mustOverride(harness, permitOverride(overrideException, "READER", "write-a", 600), `"r1"`, "routing-grant-00001")

	e.resetReceivers()
	rows := []struct {
		request string
		status  int
	}{
		{"GET /b/x HTTP/1.1\r\nHost: x\r\n\r\n", http.StatusForbidden},
		{"GET /b/%2e%2e/a/x HTTP/1.1\r\nHost: x\r\n\r\n", http.StatusBadRequest},
		{"GET /a/..%2Fb/x HTTP/1.1\r\nHost: x\r\n\r\n", http.StatusBadRequest},
		{"GET /B/x HTTP/1.1\r\nHost: x\r\n\r\n", http.StatusNotFound},
		{"DELETE /a/item HTTP/1.1\r\nHost: x\r\n\r\n", http.StatusMethodNotAllowed},
		{"GET /d HTTP/1.1\r\nHost: x\r\n\r\n", http.StatusForbidden},
	}
	for _, r := range rows {
		if code, _ := e.raw(r.request); code != r.status {
			t.Errorf("%q: %d, want %d", strings.SplitN(r.request, "\r\n", 2)[0], code, r.status)
		}
	}
	e.assertReceipts(0, 0, 0)

	// Upstream A stops: the granted write fails as an upstream failure and
	// nothing else is reached.
	e.a.server.Close()
	code, _ := e.request(http.MethodPost, "/a/item", "application/json", `{"n":1}`, bearer(t, "bob")...)
	if code != http.StatusBadGateway {
		t.Fatalf("granted write with A down: %d", code)
	}
	e.assertReceipts(0, 0, 0)
	events := e.eventsWhen(func(events []map[string]any) bool {
		for _, ev := range events {
			if ev["kind"] == "request.forwarded" && ev["decision"] == "upstream_failure" {
				return true
			}
		}
		return false
	})
	found := false
	for _, ev := range events {
		if ev["kind"] == "request.forwarded" && ev["decision"] == "upstream_failure" && ev["upstream"] == "A" && strings.Contains(fmt.Sprint(ev["generation"]), "+o1.2-") {
			found = true
		}
	}
	if !found {
		t.Fatal("no upstream failure event under the override generation")
	}
}
