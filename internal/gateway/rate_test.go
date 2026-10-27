package gateway

import (
	"errors"
	"fmt"
	"math/big"
	"math/rand/v2"
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

func rate(count, windowSeconds int64) *RateRule {
	return &RateRule{Algorithm: "sliding-window", Count: count, WindowSeconds: windowSeconds, Key: "subject"}
}

// Rate rules compile only with a supported algorithm and key, a positive
// bounded count and window, and a per-account key only on an authenticated
// operation. Any invalid rule rejects the whole policy.
func TestRateRuleCompilation(t *testing.T) {
	p := threeAPIPolicy()
	p.Operations[0].Rate = rate(10, 60)
	g, err := Compile(p, 1, time.Time{})
	if err != nil {
		t.Fatalf("valid rate rule rejected: %v", err)
	}
	_, r := g.match("/orders", "GET")
	if r.rate == nil || r.rate.count != 10 || r.rate.window != 60000 {
		t.Fatalf("compiled rule %+v", r.rate)
	}
	if _, other := g.match("/orders/1", "GET"); other.rate != nil {
		t.Fatal("a rate rule applied to an operation that does not declare one")
	}

	cases := []struct {
		name  string
		rule  RateRule
		code  string
		field string
	}{
		{"zero count", RateRule{"sliding-window", 0, 60, "subject"}, "INVALID_VALUE", "operations[0].rate.count"},
		{"negative count", RateRule{"sliding-window", -1, 60, "subject"}, "INVALID_VALUE", "operations[0].rate.count"},
		{"count above the bound", RateRule{"sliding-window", maxRateCount + 1, 60, "subject"}, "INVALID_VALUE", "operations[0].rate.count"},
		{"zero window", RateRule{"sliding-window", 10, 0, "subject"}, "INVALID_VALUE", "operations[0].rate.window_seconds"},
		{"negative window", RateRule{"sliding-window", 10, -60, "subject"}, "INVALID_VALUE", "operations[0].rate.window_seconds"},
		{"window above the bound", RateRule{"sliding-window", 10, maxRateWindowSec + 1, "subject"}, "INVALID_VALUE", "operations[0].rate.window_seconds"},
		{"token bucket", RateRule{"token-bucket", 10, 60, "subject"}, "UNSUPPORTED_FEATURE", "operations[0].rate.algorithm"},
		{"fixed window", RateRule{"fixed-window", 10, 60, "subject"}, "UNSUPPORTED_FEATURE", "operations[0].rate.algorithm"},
		{"missing algorithm", RateRule{"", 10, 60, "subject"}, "UNSUPPORTED_FEATURE", "operations[0].rate.algorithm"},
		{"unsupported client_ip key", RateRule{"sliding-window", 10, 60, "client_ip"}, "UNSUPPORTED_FEATURE", "operations[0].rate.key"},
		{"header key", RateRule{"sliding-window", 10, 60, "X-Forwarded-For"}, "UNSUPPORTED_FEATURE", "operations[0].rate.key"},
		{"missing key", RateRule{"sliding-window", 10, 60, ""}, "UNSUPPORTED_FEATURE", "operations[0].rate.key"},
	}
	for _, tc := range cases {
		p := threeAPIPolicy()
		p.Operations[0].Rate = &tc.rule
		g, err := Compile(p, 1, time.Time{})
		policyErr, ok := errors.AsType[*PolicyError](err)
		if g != nil || !ok || policyErr.Code != tc.code || policyErr.Field != tc.field {
			t.Errorf("%s: generation %v error %v, want %s at %s", tc.name, g != nil, err, tc.code, tc.field)
		}
	}

	// A public operation has no verified subject to count per account.
	p = threeAPIPolicy()
	p.Operations[3].Rate = rate(10, 60)
	if err := compileErr(p); errorCode(err) != "INVALID_VALUE" || err.(*PolicyError).Field != "operations[3].rate" {
		t.Errorf("rate rule on a public operation: %v", err)
	}

	// A valid rate rule next to an invalid body schema: nothing compiles.
	p = threeAPIPolicy()
	p.Operations[0].Rate = rate(10, 60)
	p.Operations[2].Body.Schema = []byte(`{"type":"object","additionalProperties":true,"$ref":"#"}`)
	if g, err := Compile(p, 1, time.Time{}); g != nil || errorCode(err) != "UNSUPPORTED_FEATURE" {
		t.Errorf("valid rate rule with an invalid schema: generation %v error %v", g != nil, err)
	}

	// Encodings the decoder must refuse rather than interpret.
	valid := `{"id":"x","api":"a","method":"GET","target":"/x","access":"allow","auth":"required","rate":%s}`
	documents := []struct {
		name, rate, code string
	}{
		{"count beyond int64", `{"algorithm":"sliding-window","count":1e30,"window_seconds":60,"key":"subject"}`, "SCHEMA_INVALID"},
		{"fractional count", `{"algorithm":"sliding-window","count":1.5,"window_seconds":60,"key":"subject"}`, "SCHEMA_INVALID"},
		{"count as text", `{"algorithm":"sliding-window","count":"10","window_seconds":60,"key":"subject"}`, "SCHEMA_INVALID"},
		{"repeated member", `{"algorithm":"sliding-window","count":10,"count":0,"window_seconds":60,"key":"subject"}`, "DUPLICATE_MEMBER"},
		{"unknown member", `{"algorithm":"sliding-window","count":10,"window_seconds":60,"key":"subject","burst":5}`, "UNKNOWN_FIELD"},
		{"missing window", `{"algorithm":"sliding-window","count":10,"key":"subject"}`, "INVALID_VALUE"},
	}
	for _, d := range documents {
		document := `{"schema_version":4,"profile":"core","upstreams":[{"id":"u","dial":"127.0.0.1:1"}],"apis":[{"id":"a","upstream":"u"}],` +
			`"operations":[` + fmt.Sprintf(valid, d.rate) + `]}`
		if got := errorCode(compileJSON(t, document)); got != d.code {
			t.Errorf("%s: %q, want %s", d.name, got, d.code)
		}
	}
	// A repeated rate member on one operation is the only way to declare two
	// rules for one target, and it is refused.
	twice := `{"schema_version":4,"profile":"core","upstreams":[{"id":"u","dial":"127.0.0.1:1"}],"apis":[{"id":"a","upstream":"u"}],` +
		`"operations":[{"id":"x","api":"a","method":"GET","target":"/x","access":"allow","auth":"required",` +
		`"rate":{"algorithm":"sliding-window","count":10,"window_seconds":60,"key":"subject"},` +
		`"rate":{"algorithm":"sliding-window","count":1000,"window_seconds":60,"key":"subject"}}]}`
	if got := errorCode(compileJSON(t, twice)); got != "DUPLICATE_MEMBER" {
		t.Errorf("two rules on one operation: %q", got)
	}
}

// step is one call of the limiter at a time with its expected decision.
type step struct {
	at    int64 // milliseconds
	admit bool
}

func runSteps(t *testing.T, name string, l *limiter, key counterKey, rule *rateRule, steps []step) {
	t.Helper()
	for i, s := range steps {
		got := l.admit(key, rule, s.at) == rateAdmitted
		if got != s.admit {
			t.Fatalf("%s step %d at %d ms: admitted %v, want %v", name, i, s.at, got, s.admit)
		}
	}
}

func repeatStep(n int, at int64, admit bool) []step {
	steps := make([]step, n)
	for i := range steps {
		steps[i] = step{at, admit}
	}
	return steps
}

// Hand-computed vectors for the rule: admit when C + P*(W-d)/W + 1 <= N,
// where C and P count admitted requests in the current and previous bucket
// and d is the offset into the current bucket. Each expected value is
// worked out in the comment beside it, not taken from the implementation.
func TestSlidingWindowReferenceVectors(t *testing.T) {
	key := counterKey{"op", "subject", "alice"}

	// N=10, W=60 s, buckets start at multiples of 60 000 ms.
	l := newLimiter()
	rule := &rateRule{count: 10, window: 60000}
	var steps []step
	steps = append(steps, repeatStep(10, 1000, true)...)   // bucket 0, fresh: the first 10 are admitted
	steps = append(steps, repeatStep(5, 2000, false)...)   // C=10: refused, and refusals are not counted
	steps = append(steps, step{60000, false})              // bucket 1, d=0: P=10, E = 10; 11 > 10
	steps = append(steps, repeatStep(5, 90000, true)...)   // d=30 s: E = C + 10*0.5; C+5+1 <= 10 for C = 0..4
	steps = append(steps, step{90000, false})              // C=5: 5+5+1 = 11 > 10
	steps = append(steps, step{96000, true})               // d=36 s: E = 5 + 10*0.4 = 9; 10 <= 10, equality admits
	steps = append(steps, step{96000, false})              // C=6: 6+4+1 = 11 > 10
	steps = append(steps, repeatStep(3, 119999, true)...)  // d=59.999 s: E = C + 10/60000; C+1.0002 <= 10 for C = 6..8
	steps = append(steps, step{119999, false})             // C=9: 10.0002 > 10
	steps = append(steps, step{120000, true})              // bucket 2, d=0: P=9, E = 9; 10 <= 10
	steps = append(steps, step{120000, false})             // C=1: 11 > 10
	steps = append(steps, repeatStep(10, 240000, true)...) // bucket 4 is two after bucket 2: nothing remains
	steps = append(steps, step{240000, false})
	runSteps(t, "N=10", l, key, rule, steps)

	// N=3, W=1 s: fractional weights at the equality boundary.
	l = newLimiter()
	rule = &rateRule{count: 3, window: 1000}
	runSteps(t, "N=3", l, key, rule, []step{
		{0, true}, {500, true}, {999, true}, {999, false}, // bucket 0 fills
		{1500, true},  // bucket 1, d=500: P=3, E = 1.5; 2.5 <= 3
		{1500, false}, // C=1: 1 + 1.5 + 1 = 3.5 > 3
		{1667, true},  // d=667: E = 1 + 3*0.333 = 1.999; 2.999 <= 3
		{1667, false}, // C=2: 2 + 0.999 + 1 > 3
		{1999, false}, // d=999: E = 2 + 3*0.001 = 2.003; 3.003 > 3
		{2000, true},  // bucket 2, d=0: P=2 (bucket 1 admitted two), E = 2; 3 <= 3
		{2000, false}, // C=1: 1 + 2 + 1 = 4 > 3
		{2334, false}, // d=334: E = 1 + 2*0.666 = 2.332; 3.332 > 3
		{2667, true},  // d=667: E = 1 + 2*0.333 = 1.666; 2.666 <= 3
		{2667, false}, // C=2: 2 + 0.666 + 1 > 3
	})

	// N=1, W=1 s: one request per bucket at most, and exactly at the
	// boundary the previous request still weighs fully.
	l = newLimiter()
	rule = &rateRule{count: 1, window: 1000}
	runSteps(t, "N=1", l, key, rule, []step{
		{10, true}, {999, false},
		{1000, false}, // d=0: E = 1; 2 > 1
		{1001, false}, // d=1: E = 0.999; 1.999 > 1
		{2000, true},  // previous bucket empty: E = 0
	})
}

// referenceAdmit is an independent model of the documented rule, written from
// its text with rational arithmetic and its own bucket bookkeeping.
type referenceCounter struct {
	bucket            int64
	current, previous int64
	started           bool
}

func (m *referenceCounter) admit(n, w, now int64) bool {
	bucket := now / w
	if !m.started {
		m.bucket, m.started = bucket, true
	}
	switch gap := bucket - m.bucket; {
	case gap == 1:
		m.previous, m.current, m.bucket = m.current, 0, bucket
	case gap > 1:
		m.previous, m.current, m.bucket = 0, 0, bucket
	}
	offset := max(now-m.bucket*w, 0)
	estimate := new(big.Rat).SetInt64(m.current)
	weight := big.NewRat(w-offset, w)
	estimate.Add(estimate, new(big.Rat).Mul(big.NewRat(m.previous, 1), weight))
	estimate.Add(estimate, big.NewRat(1, 1))
	if estimate.Cmp(big.NewRat(n, 1)) > 0 {
		return false
	}
	m.current++
	return true
}

// The limiter agrees with the reference model on random request sequences
// with fixed seeds, across several quotas and windows.
func TestSlidingWindowAgainstReferenceModel(t *testing.T) {
	configs := []struct{ n, w int64 }{{1, 1000}, {3, 1000}, {7, 10000}, {10, 60000}, {100, 3600000}, {maxRateCount, maxRateWindowSec * 1000}}
	for _, c := range configs {
		for seed := uint64(1); seed <= 5; seed++ {
			random := rand.New(rand.NewPCG(seed, uint64(c.n)))
			l := newLimiter()
			rule := &rateRule{count: c.n, window: c.w}
			key := counterKey{"op", "subject", "s"}
			var model referenceCounter
			now := int64(1_700_000_000_000)
			for i := range 5000 {
				// Mostly small steps, sometimes a jump of up to three windows.
				if random.IntN(50) == 0 {
					now += random.Int64N(3 * c.w)
				} else {
					now += random.Int64N(c.w/max(c.n, 1)/2 + 1)
				}
				want := model.admit(c.n, c.w, now)
				if got := l.admit(key, rule, now) == rateAdmitted; got != want {
					t.Fatalf("N=%d W=%d seed %d request %d at %d: limiter %v, reference %v", c.n, c.w, seed, i, now, got, want)
				}
			}
		}
	}
}

// A clock that moves backwards never forgives requests: the previous bucket
// then weighs fully. A forward jump only ages counts as real time would.
func TestClockMovementNeverGrantsExtraQuota(t *testing.T) {
	l := newLimiter()
	rule := &rateRule{count: 3, window: 1000}
	key := counterKey{"op", "subject", "alice"}
	runSteps(t, "clock", l, key, rule, []step{
		{5000, true}, {5100, true}, {5200, true}, {5300, false}, // bucket 5 fills
		{4000, false},                                           // backwards into bucket 4: bucket 5 is kept, E = 3
		{0, false},                                              // far backwards: the same
		{6500, true},                                            // bucket 6, d=500: P=3, E = 1.5; 2.5 <= 3
		{6500, false},                                           // C=1: 3.5 > 3
		{6000, false},                                           // d=0: E = 1 + 3; refused
		{5999, false},                                           // backwards before bucket 6: d taken as 0, E = 4
		{9000, true}, {9000, true}, {9000, true}, {9000, false}, // a forward jump past two buckets: fresh
	})
}

// Counter identity and transitions: the counter belongs to the operation and
// subject, not to the policy generation.
func TestRateCounterTransitions(t *testing.T) {
	key := counterKey{"op", "subject", "alice"}

	t.Run("count change keeps usage", func(t *testing.T) {
		l := newLimiter()
		runSteps(t, "N=10", l, key, &rateRule{10, 60000}, repeatStep(6, 1000, true))
		runSteps(t, "N=5", l, key, &rateRule{5, 60000}, []step{{2000, false}}) // 6+1 > 5
		runSteps(t, "N=8", l, key, &rateRule{8, 60000}, []step{{3000, true}, {3000, true}, {3000, false}})
	})
	t.Run("window change carries usage", func(t *testing.T) {
		l := newLimiter()
		runSteps(t, "W=60s", l, key, &rateRule{10, 60000}, repeatStep(8, 1000, true))
		// At 20 s under W=60 s the usage is 8. It becomes the current count of
		// a W=30 s counter, so only 2 more fit.
		runSteps(t, "W=30s", l, key, &rateRule{10, 30000}, []step{{20000, true}, {20000, true}, {20000, false}})
		// Next 30 s bucket, d=0: P=10, E = 10: refused.
		runSteps(t, "W=30s next bucket", l, key, &rateRule{10, 30000}, []step{{30000, false}})
	})
	t.Run("window change rounds usage up", func(t *testing.T) {
		l := newLimiter()
		runSteps(t, "W=1s", l, key, &rateRule{3, 1000}, repeatStep(3, 0, true))
		// At 1.5 s under W=1 s: P=3, d=500, usage 1.5, rounded up to 2. Under
		// W=10 s with N=3 one more fits.
		runSteps(t, "W=10s", l, key, &rateRule{3, 10000}, []step{{1500, true}, {1500, false}})
	})
	t.Run("equivalent rule keeps usage", func(t *testing.T) {
		l := newLimiter()
		runSteps(t, "first", l, key, &rateRule{3, 60000}, repeatStep(3, 1000, true))
		// A republished generation compiles a new but equal rule object.
		runSteps(t, "republished", l, key, &rateRule{3, 60000}, []step{{1000, false}})
	})
	t.Run("rule disabled and enabled keeps usage", func(t *testing.T) {
		l := newLimiter()
		runSteps(t, "enabled", l, key, &rateRule{3, 60000}, repeatStep(3, 1000, true))
		// While disabled the limiter is not called. Re-enabled within the
		// window the earlier requests still count.
		runSteps(t, "re-enabled", l, key, &rateRule{3, 60000}, []step{{30000, false}})
	})
	t.Run("keys are independent", func(t *testing.T) {
		l := newLimiter()
		rule := &rateRule{2, 60000}
		runSteps(t, "alice", l, key, rule, append(repeatStep(2, 1000, true), step{1000, false}))
		runSteps(t, "bob", l, counterKey{"op", "subject", "bob"}, rule, repeatStep(2, 1000, true))
		runSteps(t, "other operation", l, counterKey{"op2", "subject", "alice"}, rule, repeatStep(2, 1000, true))
		runSteps(t, "renamed operation", l, counterKey{"op-v2", "subject", "alice"}, rule, repeatStep(2, 1000, true))
	})
}

// Expired counters are removed without changing any decision, and a full
// table refuses new keys rather than evicting a live counter.
func TestCounterReclamationAndCapacity(t *testing.T) {
	l := newLimiter()
	l.capacity = 3
	rule := &rateRule{1, 1000}
	for i := range 3 {
		if l.admit(counterKey{"op", "subject", strconv.Itoa(i)}, rule, 0) != rateAdmitted {
			t.Fatal("first request refused")
		}
	}
	if got := l.admit(counterKey{"op", "subject", "new"}, rule, 500); got != rateNoCapacity {
		t.Fatalf("new key with a full table of live counters: %v, want no capacity", got)
	}
	// Live counters keep their counts.
	if l.admit(counterKey{"op", "subject", "0"}, rule, 500) != rateExceeded {
		t.Fatal("a live counter lost its count")
	}
	// At 1.5 s the counters are in their previous bucket: still live.
	if got := l.admit(counterKey{"op", "subject", "new"}, rule, 1500); got != rateNoCapacity {
		t.Fatalf("at 1.5 s: %v, want no capacity", got)
	}
	// From 2 s both buckets of every counter have ended; they are removed.
	if got := l.admit(counterKey{"op", "subject", "new"}, rule, 2000); got != rateAdmitted {
		t.Fatalf("after expiry: %v, want admitted", got)
	}
	if len(l.counters) != 1 {
		t.Fatalf("%d counters after reclamation, want 1", len(l.counters))
	}

	// The periodic sweep removes only expired counters.
	l = newLimiter()
	long := &rateRule{maxRateCount, 3600000}
	l.admit(counterKey{"long", "subject", "a"}, long, 0)
	for i := range sweepInterval {
		l.admit(counterKey{"short", "subject", strconv.Itoa(i)}, rule, 0)
	}
	l.admit(counterKey{"short", "subject", "late"}, rule, 5000)
	for i := range sweepInterval - 1 {
		l.admit(counterKey{"long", "subject", "a"}, long, 5000+int64(i))
	}
	if _, ok := l.counters[counterKey{"long", "subject", "a"}]; !ok || len(l.counters) > 2 {
		t.Fatalf("after sweeping: %d counters, long-window counter present %v", len(l.counters), ok)
	}
	if c := l.counters[counterKey{"long", "subject", "a"}]; c.current != sweepInterval {
		t.Fatalf("long-window counter holds %d, want %d", c.current, sweepInterval)
	}
}

// Simultaneous admissions never exceed the quota, for one key or many.
func TestConcurrentAdmissionsRespectQuota(t *testing.T) {
	l := newLimiter()
	rule := &rateRule{1000, 3600000}
	var admitted atomic.Int64
	var wg sync.WaitGroup
	for range 64 {
		wg.Go(func() {
			for range 100 {
				if l.admit(counterKey{"op", "subject", "alice"}, rule, 1000) == rateAdmitted {
					admitted.Add(1)
				}
			}
		})
	}
	wg.Wait()
	if admitted.Load() != 1000 {
		t.Fatalf("%d admitted of 6400 simultaneous requests, want exactly 1000", admitted.Load())
	}

	perKey := make([]atomic.Int64, 50)
	for i := range 50 {
		for range 4 {
			wg.Go(func() {
				for range 20 {
					if l.admit(counterKey{"op", "subject", strconv.Itoa(i)}, &rateRule{30, 3600000}, 1000) == rateAdmitted {
						perKey[i].Add(1)
					}
				}
			})
		}
	}
	wg.Wait()
	for i := range perKey {
		if perKey[i].Load() != 30 {
			t.Fatalf("subject %d: %d admitted of 80, want 30", i, perKey[i].Load())
		}
	}
}

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

// Simultaneous requests near the quota never admit more than the rule
// allows, for one subject and for many at once.
func TestConcurrentRequestsThroughGateway(t *testing.T) {
	e, _ := newRateEnv(t, func(e *env) Policy {
		p := e.ratePolicy(rate(10, 60), nil)
		return p
	}, base)

	count := func(subjects []string, perSubject int) map[string][2]int {
		var mu sync.Mutex
		results := map[string][2]int{}
		var wg sync.WaitGroup
		for _, subject := range subjects {
			token := issuer.token(t, changed(validClaims(subject), map[string]any{"roles": []string{"READER"}}))
			for range perSubject {
				wg.Go(func() {
					req, _ := http.NewRequest(http.MethodGet, "http://"+e.dataAddr+"/a/x", http.NoBody)
					req.Header.Set("Authorization", "Bearer "+token)
					resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
					if err != nil {
						t.Error(err)
						return
					}
					resp.Body.Close()
					mu.Lock()
					defer mu.Unlock()
					r := results[subject]
					switch resp.StatusCode {
					case http.StatusOK:
						r[0]++
					case http.StatusTooManyRequests:
						r[1]++
					default:
						t.Errorf("status %d", resp.StatusCode)
					}
					results[subject] = r
				})
			}
		}
		wg.Wait()
		return results
	}

	if got := count([]string{"alice"}, 40)["alice"]; got != [2]int{10, 30} {
		t.Fatalf("alice, 40 simultaneous requests: admitted/refused %v, want [10 30]", got)
	}
	e.assertReceipts(10, 0, 0)

	var load []string
	for i := range 8 {
		load = append(load, fmt.Sprintf("load-%d", i))
	}
	for subject, got := range count(load, 15) {
		if got != [2]int{10, 5} {
			t.Errorf("%s: admitted/refused %v, want [10 5]", subject, got)
		}
	}
	e.assertReceipts(10+80, 0, 0)
}

// Publication keeps counters: an equivalent republication, a count change, a
// window change, disabling and re-enabling a rule, and representative
// rollback and temporary-override publications never grant a fresh quota.
// Each request uses the rule of the generation it captured.
func TestRateCountersAcrossPublication(t *testing.T) {
	e, clock := newRateEnv(t, func(e *env) Policy { return e.ratePolicy(rate(3, 60), nil) }, base)
	revision := int64(1)
	publish := func(id string, read *RateRule) {
		t.Helper()
		outcome, code := e.publish(Change{ID: id, Expected: revision, Policy: mustJSON(e.ratePolicy(read, nil))})
		if code != http.StatusOK {
			t.Fatalf("publish %s: %d %+v", id, code, outcome)
		}
		revision = outcome.Revision
	}
	expect := func(step string, n int, want ...int) {
		t.Helper()
		if got := e.sendAs("alice", http.MethodGet, "/a/x", "", n); !statusesAre(got, want...) {
			t.Fatalf("%s: %v, want %v", step, got, want)
		}
	}

	expect("first generation", 4, 200, 200, 200, 429)
	publish("same-rule", rate(3, 60))
	expect("equivalent republication", 1, 429)
	publish("count-5", rate(5, 60))
	expect("count raised to 5", 3, 200, 200, 429)
	publish("count-2", rate(2, 60))
	expect("count lowered to 2", 1, 429)
	publish("window-120", rate(5, 120))
	expect("window changed, usage 5 carried", 1, 429)
	publish("disabled", nil)
	expect("rule removed", 2, 200, 200)
	publish("enabled", rate(5, 120))
	expect("rule restored, earlier usage kept", 1, 429)
	// Representative temporary override: raise the quota, then publish the
	// previous rule back, as an expiry or rollback would.
	publish("override", rate(7, 120))
	expect("override", 3, 200, 200, 429)
	publish("override-ended", rate(5, 120))
	expect("override ended", 1, 429)
	// Representative rollback to the first document.
	publish("rollback", rate(3, 60))
	expect("rolled back to the first rule", 1, 429)
	// Real time passing is the only thing that restores quota.
	clock.set(base + 3*60000)
	expect("two windows later", 4, 200, 200, 200, 429)
	e.assertReceipts(3+2+2+2+3, 0, 0)
}

// A request paused after capture keeps the rate rule of its generation even
// when a generation with another rule is published meanwhile.
func TestInFlightRequestKeepsCapturedRateRule(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.ratePolicy(rate(1, 60), nil), seedOptions{})
	clock := newControlledClock(t, base)
	dir := t.TempDir()
	holdFile, release := filepath.Join(dir, "captured"), filepath.Join(dir, "release")
	e.start(binaries.fault, clock.env(), "GATEWAY_HOLD_PATH=/a/held", "GATEWAY_HOLD_FILE="+holdFile, "GATEWAY_HOLD_RELEASE="+release)

	if got := e.sendAs("alice", http.MethodGet, "/a/x", "", 1); !statusesAre(got, 200) {
		t.Fatalf("first request: %v", got)
	}
	result := make(chan int, 1)
	go func() {
		code, _ := e.request(http.MethodGet, "/a/held", "", "", bearer(t, "alice")...)
		result <- code
	}()
	waitForFile(t, holdFile)
	if outcome, code := e.publish(Change{ID: "count-10", Expected: 1, Policy: mustJSON(e.ratePolicy(rate(10, 60), nil))}); code != http.StatusOK {
		t.Fatalf("publish: %d %+v", code, outcome)
	}
	os.WriteFile(release, nil, 0o600)
	if code := <-result; code != http.StatusTooManyRequests {
		t.Fatalf("held request under the count-1 generation: %d, want 429", code)
	}
	if got := e.sendAs("alice", http.MethodGet, "/a/x", "", 1); !statusesAre(got, 200) {
		t.Fatalf("new request under the count-10 generation: %v", got)
	}
	e.assertReceipts(2, 0, 0)
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s never appeared", path)
}

// Counters live in the gateway process: a restart starts them empty, and no
// endpoint on either listener resets them.
func TestRateCountersAndRestart(t *testing.T) {
	e, _ := newRateEnv(t, func(e *env) Policy { return e.ratePolicy(rate(2, 60), nil) }, base)
	if got := e.sendAs("alice", http.MethodGet, "/a/x", "", 3); !statusesAre(got, 200, 200, 429) {
		t.Fatalf("before reset attempts: %v", got)
	}
	for _, path := range []string{"/reset", "/rate/reset", "/counters"} {
		for _, method := range []string{http.MethodPost, http.MethodDelete} {
			if code, _ := e.adminRaw(method, path, ""); code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
				t.Errorf("controller %s %s: %d", method, path, code)
			}
			if code, _ := e.request(method, path, "", ""); code != http.StatusNotFound {
				t.Errorf("data plane %s %s: %d", method, path, code)
			}
		}
	}
	if got := e.sendAs("alice", http.MethodGet, "/a/x", "", 1); !statusesAre(got, 429) {
		t.Fatalf("after reset attempts: %v", got)
	}

	clock := newControlledClock(t, base)
	e.start(binaries.fault, clock.env())
	if got := e.sendAs("alice", http.MethodGet, "/a/x", "", 3); !statusesAre(got, 200, 200, 429) {
		t.Fatalf("after restart: %v", got)
	}
	e.assertReceipts(4, 0, 0)
}
