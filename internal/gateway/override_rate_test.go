// Rate overrides keep counters.

package gateway

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/policy"
	"github.com/Alex-Zechariah-02/apiward/internal/store"
)

// Rate overrides change only the count of an existing rule. The counters
// already accrued keep applying across creation, revocation and expiry, so
// no change grants a fresh quota, and subjects keep separate counters. The
// expected decisions are worked out by hand from the admission rule
// (admit when C + P*(W-d)/W + 1 <= N), not taken from the implementation.
func TestRateOverrideKeepsCounters(t *testing.T) {
	ratePolicy := func(e *env) policy.Policy {
		p := e.protectedPolicy()
		p.Roles = append(p.Roles, policy.Role{ID: "TRIAL"})
		p.Permits = append(p.Permits, policy.Permit{Role: "TRIAL", Operation: "read-a"})
		p.Operations[0].Rate = rate(4, 10) // read-a: 4 per 10 s per subject
		return p
	}
	e, clock := newRateEnv(t, ratePolicy, base)
	harness := e.mustLogin(harnessAdmin, harnessPassword)

	// Bucket [base, base+10 s). alice uses 3 of 4.
	if got := e.sendAs("alice", http.MethodGet, "/a/x", "", 3); !statusesAre(got, 200, 200, 200) {
		t.Fatalf("alice under the base rule: %v", got)
	}
	// Exception to 6 mid-window: C=3, so 3 more (C+1 <= 6 for C = 3..5).
	raise := e.mustOverride(harness, rateOverride(store.OverrideException, "read-a", 6, 600), `"r1"`, "rate-raise-000001")
	if got := e.sendAs("alice", http.MethodGet, "/a/x", "", 4); !statusesAre(got, 200, 200, 200, 429) {
		t.Fatalf("alice under the exception: %v, want 3 admitted then 429 (no reset)", got)
	}
	// Revocation: the base count 4 applies to C=6, so nothing until the
	// window moves on; no quota is granted back.
	if code, _, body := e.revokeOverride(harness, raise.Override, "rate-raise-revoke"); code != http.StatusOK {
		t.Fatalf("revocation: %d %s", code, body)
	}
	if got := e.sendAs("alice", http.MethodGet, "/a/x", "", 2); !statusesAre(got, 429, 429) {
		t.Fatalf("alice after the revocation: %v", got)
	}
	// bob, another subject, has his own counter: 4 under the base rule.
	if got := e.sendAs("bob", http.MethodGet, "/a/x", "", 5); !statusesAre(got, 200, 200, 200, 200, 429) {
		t.Fatalf("bob: %v", got)
	}

	// Next bucket, d = 5 s: alice has P=6, C=0, so E = 6*(10-5)/10 = 3.
	clock.set(base + 15_000)
	// Containment to 2: alice 3+1 > 2, refused; carol, who has sent
	// nothing, gets 2 (0+1 <= 2, 1+1 <= 2, then 2+1 > 2).
	contain := e.mustOverride(harness, rateOverride(store.OverrideContainment, "read-a", 2, 600), `"r1"`, "rate-lower-000001")
	if got := e.sendAs("alice", http.MethodGet, "/a/x", "", 1); !statusesAre(got, 429) {
		t.Fatalf("alice under the containment: %v", got)
	}
	if got := e.sendAs("carol", http.MethodGet, "/a/x", "", 3); !statusesAre(got, 200, 200, 429) {
		t.Fatalf("carol under the containment: %v", got)
	}
	// Revoking the containment restores 4: alice E = 3 → one more (3+1 <= 4);
	// carol C=2 → two more.
	if code, _, body := e.revokeOverride(harness, contain.Override, "rate-lower-revoke"); code != http.StatusOK {
		t.Fatalf("revocation: %d %s", code, body)
	}
	if got := e.sendAs("alice", http.MethodGet, "/a/x", "", 2); !statusesAre(got, 200, 429) {
		t.Fatalf("alice after the containment ended: %v", got)
	}
	if got := e.sendAs("carol", http.MethodGet, "/a/x", "", 3); !statusesAre(got, 200, 200, 429) {
		t.Fatalf("carol after the containment ended: %v", got)
	}
	// Forged identity headers neither choose another counter nor a count.
	if got := e.sendAs("alice", http.MethodGet, "/a/x", "", 1, "X-Gateway-Subject", "nobody", "X-Gateway-Rate", "100"); !statusesAre(got, 429) {
		t.Fatalf("alice with forged headers: %v", got)
	}
	// A refused request (writes need WRITER; bob has READER) never reaches the
	// counter, so it cannot use or reset anyone's quota.
	if got := e.sendAs("bob", http.MethodPost, "/a/item", `{"n":1}`, 3); !statusesAre(got, 403, 403, 403) {
		t.Fatalf("bob writes: %v", got)
	}

	// Concurrency at the boundary: a fresh window (two buckets later), an
	// exception to 5, and 20 simultaneous requests by one subject: exactly 5.
	clock.set(base + 40_000)
	race := e.mustOverride(harness, rateOverride(store.OverrideException, "read-a", 5, 600), `"r1"`, "rate-race-0000001")
	e.resetReceivers()
	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted := 0
	start := make(chan struct{})
	token := "Bearer " + issuer.tokenFor(t, "alice")
	for range 20 {
		wg.Go(func() {
			<-start
			code, _ := e.request(http.MethodGet, "/a/x", "", "", "Authorization", token)
			if code == http.StatusOK {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		})
	}
	close(start)
	wg.Wait()
	if admitted != 5 || e.a.count() != 5 {
		t.Fatalf("%d admitted, %d received; want 5", admitted, e.a.count())
	}
	if code, _, body := e.revokeOverride(harness, race.Override, "rate-race-revoke"); code != http.StatusOK {
		t.Fatalf("revocation: %d %s", code, body)
	}

	// Expiry mid-window after an unrelated permanent change. A fresh window
	// (bucket base+80 s is more than one after base+40 s), an exception to 6
	// that expires in 2 s, then a new subject is added to the base.
	clock.set(base + 80_000)
	expiring := e.mustOverride(harness, rateOverride(store.OverrideException, "read-a", 6, 2), `"r1"`, "rate-expiring-0001")
	next := ratePolicy(e)
	next.Roles = append(next.Roles, policy.Role{ID: "OTHER"})
	e.confirmPolicy(harness, next, 1, "rate-permanent-change")
	if got := e.sendAs("alice", http.MethodGet, "/a/x", "", 5); !statusesAre(got, times(5, 200)...) {
		t.Fatalf("alice under the expiring exception: %v", got)
	}
	waitFor(t, expiring.Expires.Add(5*time.Second), "the rate exception to expire", func() bool { return len(e.status().Overrides) == 0 })
	// The base count 4 applies to the 5 already counted: no fresh quota.
	if got := e.sendAs("alice", http.MethodGet, "/a/x", "", 1); !statusesAre(got, 429) {
		t.Fatalf("alice after the expiry: %v", got)
	}
	// Dave's READER role, from his token, keeps its own counter under the
	// base count after the expiry.
	if got := e.sendAs("dave", http.MethodGet, "/a/x", "", 5); !statusesAre(got, 200, 200, 200, 200, 429) {
		t.Fatalf("dave after the expiry: %v", got)
	}
	if s := e.status(); s.Confirmed != 2 || !strings.HasPrefix(s.Generation, "r2-") {
		t.Fatalf("status after the expiry %+v", s)
	}

	// The rule's window, key and algorithm never changed: every generation
	// served the base's 10 s subject-keyed sliding window.
	events := e.eventsWhen(func(events []map[string]any) bool { return countKind(events, "tpo.revoked") >= 2 })
	for _, ev := range events {
		if ev["kind"] == "request.rejected" && ev["stage"] == "RATE" && ev["operation"] != "read-a" {
			t.Fatalf("rate refusal on another operation: %v", ev)
		}
	}
}
