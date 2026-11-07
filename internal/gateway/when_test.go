// Host and header conditions: the compiler's collision and weaker-variant
// refusals, the selection of a variant, and real requests through the data
// plane, where a condition chooses an operation and never grants access.

package gateway

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/compile"
	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

// variant adds a copy of the operation at index i of threeAPIPolicy with the
// given conditions, so it differs from its sibling only where a case says.
func variant(p *policy.Policy, i int, id string, when policy.When) *policy.Operation {
	op := p.Operations[i]
	op.ID, op.When = id, &when
	p.Operations = append(p.Operations, op)
	return &p.Operations[len(p.Operations)-1]
}

func TestWhenCompile(t *testing.T) {
	v2 := policy.When{Headers: []policy.HeaderCondition{{Name: "X-Version", Value: "2"}}}
	host := policy.When{Hosts: []string{"*.example.com"}}
	for name, tc := range map[string]struct {
		change func(p *policy.Policy)
		code   string
	}{
		"equal conditions": {func(p *policy.Policy) {
			variant(p, 0, "orders.v2", v2)
			variant(p, 0, "orders.v2b", policy.When{Headers: []policy.HeaderCondition{{Name: "x-version", Value: "2"}}})
		}, "TARGET_COLLISION"},
		"overlapping, same count": {func(p *policy.Policy) {
			variant(p, 0, "orders.v2", v2)
			variant(p, 0, "orders.host", host)
		}, "TARGET_COLLISION"},
		"public variant": {func(p *policy.Policy) { variant(p, 0, "orders.v2", v2).Auth = "none" }, "CONDITION_WEAKER"},
		"extra role": {func(p *policy.Policy) {
			variant(p, 0, "orders.v2", v2)
			p.Permits = append(p.Permits, policy.Permit{Role: "CUSTOMER", Operation: "orders.v2"})
		}, "CONDITION_WEAKER"},
		"missing rate": {func(p *policy.Policy) {
			p.Operations[0].Rate = &policy.RateRule{Algorithm: "sliding-window", Count: 5, WindowSeconds: 60, Key: "subject"}
			variant(p, 0, "orders.v2", v2).Rate = nil
		}, "CONDITION_WEAKER"},
		"body where the sibling refuses one": {func(p *policy.Policy) {
			variant(p, 0, "orders.v2", v2).Body = jsonBody(1024, anyObject)
		}, "CONDITION_WEAKER"},
		"allowed where the sibling is denied": {func(p *policy.Policy) {
			variant(p, 5, "reports.v2", v2).Access = "allow"
		}, "CONDITION_WEAKER"},
		"gateway header": {func(p *policy.Policy) {
			variant(p, 0, "orders.v2", policy.When{Headers: []policy.HeaderCondition{{Name: "X-Gateway-Route", Value: "1"}}})
		}, "INVALID_VALUE"},
		"host with a port": {func(p *policy.Policy) {
			variant(p, 0, "orders.v2", policy.When{Hosts: []string{"api.example.com:8080"}})
		}, "INVALID_VALUE"},
		"no condition": {func(p *policy.Policy) { variant(p, 0, "orders.v2", policy.When{}) }, "INVALID_VALUE"},
	} {
		p := threeAPIPolicy()
		tc.change(&p)
		err := compileErr(p)
		if invalid, ok := errors.AsType[*policy.PolicyError](err); !ok || invalid.Code != tc.code {
			t.Errorf("%s: %v, want %s", name, err, tc.code)
		}
	}

	// A stricter variant compiles; the variant with the most conditions that
	// hold is selected, the unconditioned operation otherwise.
	p := threeAPIPolicy()
	variant(&p, 0, "orders.v2", v2).Access = "deny"
	both := variant(&p, 0, "orders.both", policy.When{Hosts: host.Hosts, Headers: v2.Headers})
	both.Rate = &policy.RateRule{Algorithm: "sliding-window", Count: 5, WindowSeconds: 60, Key: "subject"}
	g, err := compile.Compile(p, 1, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		host   string
		header http.Header
		want   string
	}{
		{"shop.test", nil, "orders.list"},
		{"shop.test", http.Header{"X-Version": {"2"}}, "orders.v2"},
		{"API.Example.com:8443", http.Header{"X-Version": {"2"}}, "orders.both"},
		{"a.b.example.com", http.Header{"X-Version": {"2"}}, "orders.both"},
		{"example.com", http.Header{"X-Version": {"2"}}, "orders.v2"},
		{"shop.test", http.Header{"X-Version": {"2", "2"}}, "orders.list"},
		{"shop.test", http.Header{"X-Version": {"3"}}, "orders.list"},
	} {
		_, rule := g.Match("/orders", http.MethodGet, compile.Inbound{Host: tc.host, Header: tc.header})
		if rule == nil || rule.Operation() != tc.want {
			t.Errorf("%s %v selected %v, want %s", tc.host, tc.header, rule, tc.want)
		}
	}
}

// Through the real data plane a header selects the denied variant, and on a
// target with only a conditioned operation a request without the condition
// finds no operation: neither path reaches the upstream.
func TestWhenNeverGrants(t *testing.T) {
	e := newEnv(t)
	p := e.policy(e.a, false)
	v2 := policy.When{Headers: []policy.HeaderCondition{{Name: "X-Version", Value: "2"}}}
	p.Operations = append(p.Operations,
		policy.Operation{ID: "read-a-v2", API: "a", Method: "GET", Target: "/a/*", Access: "deny", Auth: "none", When: &v2},
		policy.Operation{ID: "only-v2", API: "a", Method: "GET", Target: "/v2", Access: "allow", Auth: "none", When: &v2})
	e.serveNative(p, forwardTimeouts)

	if status, _ := e.get("/a/x"); status != http.StatusOK {
		t.Fatalf("unconditioned operation: %d", status)
	}
	if status, _ := e.get("/a/x", "X-Version", "2"); status != http.StatusForbidden {
		t.Fatalf("denied variant: %d", status)
	}
	if status, _ := e.get("/v2"); status != http.StatusNotFound {
		t.Fatalf("condition not met: %d", status)
	}
	if status, _ := e.get("/v2", "X-Version", "2"); status != http.StatusOK {
		t.Fatalf("condition met: %d", status)
	}
	e.assertReceipts(2, 0, 0)
}

// A conditioned operation counts on its unconditioned sibling's counter, so
// alternating a header cannot exceed the plain operation's quota.
func TestWhenSharesCounter(t *testing.T) {
	e := newEnv(t)
	p := e.policy(e.a, false)
	p.Operations[0].Rate = &policy.RateRule{Algorithm: "sliding-window", Count: 2, WindowSeconds: 60, Key: "client_address"}
	variant(&p, 0, "read-a-v2", policy.When{Headers: []policy.HeaderCondition{{Name: "X-Version", Value: "2"}}})
	e.serveNative(p, forwardTimeouts)
	for i, want := range []int{http.StatusOK, http.StatusOK, http.StatusTooManyRequests, http.StatusTooManyRequests} {
		header := []string{}
		if i%2 == 1 {
			header = []string{"X-Version", "2"}
		}
		if status, _ := e.get("/a/x", header...); status != want {
			t.Fatalf("request %d: %d, want %d", i+1, status, want)
		}
	}
}
