// Overrides with concurrent activation, rate rules and load.

package gateway

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

// An activation and an override creation sent at the same moment are
// decided in one order: if the activation is first, the creation is refused
// as provisional; if the creation is first, the conflicting candidate is
// refused. Confirmation, rollback and an override's expiry racing at a
// provisional deadline give each its single final disposition.
func TestConcurrentOverridesAndActivation(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
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
			_, activated = e.provisionalFrom(harness, e.protectedPolicy(), confirmed, 2, fmt.Sprintf("race-deadline-b-%02d", round))
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
	ratePolicy := func(e *env) policy.Policy {
		p := e.protectedPolicy()
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

// Under continuous traffic, overrides are created and revoked repeatedly:
// every request is decided and forwarded by one generation, and a write only
// the grant permits reaches the upstream only under a generation that has
// the grant.
func TestOverridesUnderLoad(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
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
