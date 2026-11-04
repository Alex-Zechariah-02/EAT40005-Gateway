// Rate limits through the gateway.

package gateway

import (
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ratePolicy is the protected test policy with a rate rule on each of its
// authenticated operations and a TRIAL role for a third subject.
func (e *env) ratePolicy(read, write *RateRule) Policy {
	p := e.protectedPolicy()
	p.Roles = append(p.Roles, Role{ID: "TRIAL"})
	p.Permits = append(p.Permits, Permit{Role: "TRIAL", Operation: "read-a"})
	for i := range p.Operations {
		switch p.Operations[i].ID {
		case "read-a":
			p.Operations[i].Rate = read
		case "write-a":
			p.Operations[i].Rate = write
		}
	}
	return p
}

// controlledClock makes a fault-injection gateway read its limiter time from
// a file the test writes.
type controlledClock struct {
	t    *testing.T
	path string
}

func newControlledClock(t *testing.T, at int64) *controlledClock {
	c := &controlledClock{t: t, path: filepath.Join(t.TempDir(), "rate-clock")}
	c.set(at)
	return c
}

func (c *controlledClock) set(ms int64) {
	if err := os.WriteFile(c.path, []byte(strconv.FormatInt(ms, 10)), 0o600); err != nil {
		c.t.Fatal(err)
	}
}

func (c *controlledClock) env() string { return "GATEWAY_RATE_CLOCK=" + c.path }

// sendAs sends n identical requests for subject and returns their statuses.
func (e *env) sendAs(subject, method, path, body string, n int, header ...string) []int {
	e.t.Helper()
	contentType := ""
	if body != "" {
		contentType = "application/json"
	}
	token := issuer.tokenFor(e.t, subject)
	statuses := make([]int, n)
	for i := range statuses {
		statuses[i], _ = e.request(method, path, contentType, body, append([]string{"Authorization", "Bearer " + token}, header...)...)
	}
	return statuses
}

func statusesAre(got []int, want ...int) bool { return slices.Equal(got, want) }

func times(n, status int) []int {
	statuses := make([]int, n)
	for i := range statuses {
		statuses[i] = status
	}
	return statuses
}

// The production gateway admits the first N requests of a subject, refuses
// the next with 429 without forwarding it, and counts subjects and
// operations separately. Forged identity, forwarding and rate-key headers
// change nothing.
func TestRateLimitThroughGateway(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.ratePolicy(rate(3, maxRateWindowSec), rate(2, maxRateWindowSec)), seedOptions{})
	e.start(binaries.plain)

	if got := e.sendAs("alice", http.MethodGet, "/a/x", "", 4); !statusesAre(got, 200, 200, 200, 429) {
		t.Fatalf("alice reads: %v, want 200 200 200 429", got)
	}
	e.assertReceipts(3, 0, 0)

	// The refusal: 429, a RATE rejection naming the verified subject, and no
	// stage after RATE evaluated.
	resp, body := e.exchange(http.MethodGet, "/a/x", "", "", bearer(t, "alice")...)
	_, id := requestID(resp, body)
	events := e.requestEvents(id, 1)
	want := expectedStages([]string{"INGRESS", "CAPTURE", "CANONICAL", "ROUTE", "ACCESS", "JWT", "RBAC"}, "RATE", stageReject)
	if resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(body, `"rate_limited"`) || len(events) != 1 ||
		events[0]["stage"] != "RATE" || events[0]["reason"] != "quota_exceeded" || events[0]["subject"] != "alice" ||
		events[0]["operation"] != "read-a" || !sameStages(events[0]["stages"], want) {
		t.Fatalf("refusal: %d %s, events %v", resp.StatusCode, body, events)
	}

	// Another subject has its own quota on the same operation.
	if got := e.sendAs("bob", http.MethodGet, "/a/x", "", 4); !statusesAre(got, 200, 200, 200, 429) {
		t.Fatalf("bob reads: %v", got)
	}
	// Another operation has its own quota for the same subject.
	if got := e.sendAs("alice", http.MethodPost, "/a/item", `{"n":1}`, 3); !statusesAre(got, 200, 200, 429) {
		t.Fatalf("alice writes: %v", got)
	}
	e.assertReceipts(3+3+2, 0, 0)

	// Headers a client might use to choose another counter.
	forged := []string{
		"X-Gateway-Subject", "carol", "X-Forwarded-For", "203.0.113.9", "Forwarded", "for=203.0.113.9",
		"X-Real-Ip", "203.0.113.9", "X-Rate-Key", "carol", "X-Gateway-Rate", "reset", "X-Gateway-Validated", "true",
	}
	if got := e.sendAs("alice", http.MethodGet, "/a/x", "", 2, forged...); !statusesAre(got, 429, 429) {
		t.Fatalf("alice with forged headers: %v", got)
	}
	// Carol, claiming to be alice, is counted as carol and forwarded as carol.
	e.resetReceivers()
	claim := []string{"X-Gateway-Subject", "alice", "X-Forwarded-For", "198.51.100.1"}
	if got := e.sendAs("carol", http.MethodGet, "/a/x", "", 4, claim...); !statusesAre(got, 200, 200, 200, 429) {
		t.Fatalf("carol: %v", got)
	}
	for _, r := range e.a.receipts() {
		if r.Headers.Get("X-Gateway-Subject") != "carol" {
			t.Fatalf("upstream saw subject %q", r.Headers.Get("X-Gateway-Subject"))
		}
	}
	e.assertReceipts(3, 0, 0)
}

// newRateEnv starts a fault-injection gateway whose limiter reads the
// controlled clock.
func newRateEnv(t *testing.T, policy func(e *env) Policy, at int64) (*env, *controlledClock) {
	e := newEnv(t)
	seed(t, e.database, policy(e), seedOptions{})
	clock := newControlledClock(t, at)
	e.start(binaries.fault, clock.env())
	return e, clock
}

// base is a controlled time at the start of a 10 s bucket.
const base = int64(1_800_000_000_000)

// The weighted estimate across a bucket boundary matches the reference
// vectors on the real gateway, with receipts counted at the upstream.
func TestWeightedWindowThroughGateway(t *testing.T) {
	e, clock := newRateEnv(t, func(e *env) Policy { return e.ratePolicy(rate(4, 10), nil) }, base+1000)

	if got := e.sendAs("alice", http.MethodGet, "/a/x", "", 6); !statusesAre(got, 200, 200, 200, 200, 429, 429) {
		t.Fatalf("first bucket: %v", got)
	}
	// Refused requests are not counted: d=5 s in the next bucket, P=4,
	// E = C + 2, admitted while C+2+1 <= 4, so twice.
	clock.set(base + 15000)
	if got := e.sendAs("alice", http.MethodGet, "/a/x", "", 3); !statusesAre(got, 200, 200, 429) {
		t.Fatalf("half-way through the next bucket: %v", got)
	}
	// d=7.5 s: P=4 weighs 1, C=2: 2+1+1 = 4 <= 4, admitted once (equality).
	clock.set(base + 17500)
	if got := e.sendAs("alice", http.MethodGet, "/a/x", "", 2); !statusesAre(got, 200, 429) {
		t.Fatalf("three quarters through: %v", got)
	}
	// Two buckets later every count has expired.
	clock.set(base + 30000)
	if got := e.sendAs("alice", http.MethodGet, "/a/x", "", 5); !statusesAre(got, 200, 200, 200, 200, 429) {
		t.Fatalf("after expiry: %v", got)
	}
	e.assertReceipts(4+2+1+4, 0, 0)

	// A clock moved backwards forgives nothing.
	clock.set(base)
	if got := e.sendAs("alice", http.MethodGet, "/a/x", "", 2); !statusesAre(got, 429, 429) {
		t.Fatalf("clock moved backwards: %v", got)
	}
	e.assertReceipts(11, 0, 0)
}

// Requests refused by JWT or RBAC never reach the limiter, so they consume
// nothing.
func TestEarlierRejectionsConsumeNoQuota(t *testing.T) {
	e, _ := newRateEnv(t, func(e *env) Policy { return e.ratePolicy(rate(2, 60), rate(2, 60)) }, base)

	expired := issuer.token(t, changed(validClaims("alice"), map[string]any{"exp": time.Now().Add(-time.Minute).Unix()}))
	for range 5 {
		if code, _ := e.request(http.MethodGet, "/a/x", "", "", "Authorization", "Bearer "+expired); code != http.StatusUnauthorized {
			t.Fatalf("expired token: %d", code)
		}
	}
	// Carol holds TRIAL, which has no permit for write-a: refused by RBAC.
	if got := e.sendAs("carol", http.MethodPost, "/a/item", `{"n":1}`, 5); !statusesAre(got, times(5, 403)...) {
		t.Fatalf("carol before the permit: %v", got)
	}
	if got := e.sendAs("alice", http.MethodGet, "/a/x", "", 3); !statusesAre(got, 200, 200, 429) {
		t.Fatalf("alice after refused tokens: %v", got)
	}
	// Grant TRIAL write-a; carol's quota is untouched by the refusals.
	p := e.ratePolicy(rate(2, 60), rate(2, 60))
	p.Permits = append(p.Permits, Permit{Role: "TRIAL", Operation: "write-a"})
	if outcome, code := e.publish(Change{ID: "grant", Expected: 1, Policy: mustJSON(p)}); code != http.StatusOK {
		t.Fatalf("publish: %d %+v", code, outcome)
	}
	if got := e.sendAs("carol", http.MethodPost, "/a/item", `{"n":1}`, 3); !statusesAre(got, 200, 200, 429) {
		t.Fatalf("carol after the permit: %v", got)
	}
	e.assertReceipts(4, 0, 0)
}
