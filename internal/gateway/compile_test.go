// Policy compiler references, targets, specificity and roles.

package gateway

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

func TestInvalidReferencesAndConflictsAreRejected(t *testing.T) {
	cases := []struct {
		name   string
		change func(p *policy.Policy)
		code   string
		field  string
	}{
		{"unknown API", func(p *policy.Policy) { p.Operations[0].API = "billing" }, "UNKNOWN_REFERENCE", "operations[0].api"},
		{"unknown upstream", func(p *policy.Policy) { p.APIs[1].Upstream = "elsewhere" }, "UNKNOWN_REFERENCE", "apis[1].upstream"},
		{"unknown role", func(p *policy.Policy) { p.Permits[0].Role = "ADMIN" }, "UNKNOWN_REFERENCE", "permits[0].role"},
		{"unknown operation", func(p *policy.Policy) { p.Permits[1].Operation = "orders.delete" }, "UNKNOWN_REFERENCE", "permits[1].operation"},
		{"permit on public operation", func(p *policy.Policy) { p.Permits[0].Operation = "inventory.health" }, "INVALID_VALUE", "permits[0].operation"},
		{"duplicate permit", func(p *policy.Policy) { p.Permits = append(p.Permits, p.Permits[0]) }, "DUPLICATE_ID", "permits[2]"},
		{"duplicate operation id", func(p *policy.Policy) { p.Operations[1].ID = "orders.list" }, "DUPLICATE_ID", "operations[1].id"},
		{"duplicate API id", func(p *policy.Policy) { p.APIs[2].ID = "orders" }, "DUPLICATE_ID", "apis[2].id"},
		{"duplicate upstream id", func(p *policy.Policy) { p.Upstreams[2].ID = "orders" }, "DUPLICATE_ID", "upstreams[2].id"},
		{"duplicate role", func(p *policy.Policy) { p.Roles[1].ID = "CUSTOMER" }, "DUPLICATE_ID", "roles[1].id"},
		{"same target and method twice", func(p *policy.Policy) {
			p.Operations = append(p.Operations, policy.Operation{ID: "orders.list2", API: "inventory", Method: "GET", Target: "/orders", Access: "deny", Auth: "none"})
		}, "DUPLICATE_OPERATION", "operations[6]"},
		{"same prefix and method twice", func(p *policy.Policy) {
			p.Operations = append(p.Operations, policy.Operation{ID: "orders.get2", API: "orders", Method: "GET", Target: "/orders/*", Access: "allow", Auth: "none"})
		}, "DUPLICATE_OPERATION", "operations[6]"},
		{"two upstreams with one destination", func(p *policy.Policy) { p.Upstreams[1].Dial = p.Upstreams[0].Dial }, "INVALID_UPSTREAM", "upstreams[1].dial"},
		{"identifier with a space", func(p *policy.Policy) { p.Operations[0].ID = "orders list" }, "INVALID_VALUE", "operations[0].id"},
		{"identifier with a newline", func(p *policy.Policy) { p.Roles[0].ID = "CUSTOMER\n" }, "INVALID_VALUE", "roles[0].id"},
		{"empty identifier", func(p *policy.Policy) { p.APIs[0].ID = "" }, "INVALID_VALUE", "apis[0].id"},
		{"unknown access value", func(p *policy.Policy) { p.Operations[0].Access = "permit" }, "INVALID_VALUE", "operations[0].access"},
		{"unknown auth value", func(p *policy.Policy) { p.Operations[0].Auth = "optional" }, "INVALID_VALUE", "operations[0].auth"},
		{"zero body limit", func(p *policy.Policy) { p.Operations[2].Body.MaxBytes = 0 }, "INVALID_VALUE", "operations[2].body.max_bytes"},
	}
	for _, tc := range cases {
		p := threeAPIPolicy()
		tc.change(&p)
		err := compileErr(p)
		policyErr, ok := errors.AsType[*policy.PolicyError](err)
		if !ok || policyErr.Code != tc.code || policyErr.Field != tc.field {
			t.Errorf("%s: %v, want %s at %s", tc.name, err, tc.code, tc.field)
		}
	}
}

// The former membership attack vectors now fail at the removed policy field;
// sub and roles themselves are validated at the verified-token boundary.
func TestPolicySubjectsRejected(t *testing.T) {
	for _, subject := range []Subject{
		{ID: "s", Status: "active", Roles: []string{"ADMIN"}},
		{ID: "s", Status: "active", Roles: []string{"customer"}},
		{ID: "s", Status: "active", Roles: []string{"STAFF", "STAFF"}},
		{ID: "cust-001", Status: "active"},
		{ID: "s", Status: "suspended"},
		{ID: "s"},
		{Status: "active"},
		{ID: "cust 001", Status: "active"},
		{ID: "cust\n001", Status: "active"},
		{ID: "café", Status: "active"},
		{ID: strings.Repeat("s", policy.MaxSubjectLength+1), Status: "active"},
	} {
		if _, err := policy.DecodePolicy(withSubject(threeAPIPolicy(), subject)); errorCode(err) != "UNKNOWN_FIELD" {
			t.Errorf("legacy membership %+v: %v", subject, err)
		}
	}
}

func TestInvalidTargetsMethodsAndDestinations(t *testing.T) {
	targets := []string{"", "orders", "/a*", "/a/*/b", "*", "/*/", "/a/**", "/a//b", "/a/./b", "/a/../b", "/a%2Fb", "/a b", "/a\\b", "/café"}
	for _, target := range targets {
		p := threeAPIPolicy()
		p.Operations[0].Target = target
		if got := errorCode(compileErr(p)); got != "INVALID_TARGET" {
			t.Errorf("target %q: %q, want INVALID_TARGET", target, got)
		}
	}

	methods := []string{"get", "Get", "TRACE", "CONNECT", "PROPFIND", ""}
	for _, method := range methods {
		p := threeAPIPolicy()
		p.Operations[0].Method = method
		if got := errorCode(compileErr(p)); got != "INVALID_METHOD" {
			t.Errorf("method %q: %q, want INVALID_METHOD", method, got)
		}
	}

	dials := []string{
		"10.0.0.5:80", "192.168.1.1:80", "0.0.0.0:80", "[::]:80", "localhost:80", "127.0.0.1", "127.0.0.1:0",
		"127.0.0.1:99999", "http://127.0.0.1:80", "{http.request.host}:80", "127.0.0.1:80/x", "example.com:443",
	}
	for _, dial := range dials {
		p := threeAPIPolicy()
		p.Upstreams[0].Dial = dial
		if got := errorCode(compileErr(p)); got != "INVALID_UPSTREAM" {
			t.Errorf("dial %q: %q, want INVALID_UPSTREAM", dial, got)
		}
	}
}

// The selected operation does not depend on declaration order.
func TestSpecificityIgnoresDeclarationOrder(t *testing.T) {
	p := threeAPIPolicy()
	p.Operations = append(p.Operations,
		policy.Operation{ID: "all", API: "reports", Method: "GET", Target: "/*", Access: "allow", Auth: "none"},
		policy.Operation{ID: "items", API: "inventory", Method: "GET", Target: "/inventory/*", Access: "allow", Auth: "none"},
		policy.Operation{ID: "special", API: "inventory", Method: "GET", Target: "/inventory/items/special", Access: "allow", Auth: "none"},
	)
	requests := []struct{ path, operation string }{
		{"/orders", "orders.list"},
		{"/orders/7", "orders.get"},
		{"/inventory/items/7", "inventory.get"},
		{"/inventory/items/special", "special"},
		{"/inventory/health", "inventory.health"},
		{"/inventory/other", "items"},
		{"/anything/else", "all"},
	}
	var reference []string
	for round := range 20 {
		shuffled := slices.Clone(p.Operations)
		// Deterministic permutation per round: rotate and reverse.
		shuffled = append(shuffled[round%len(shuffled):], shuffled[:round%len(shuffled)]...)
		if round%2 == 1 {
			slices.Reverse(shuffled)
		}
		q := p
		q.Operations = shuffled
		g, err := Compile(q, 1, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, r := range requests {
			_, rule := g.match(r.path, "GET")
			if rule == nil {
				got = append(got, "")
				continue
			}
			got = append(got, rule.operation)
		}
		if round == 0 {
			reference = got
			for i, r := range requests {
				if got[i] != r.operation {
					t.Errorf("%s selected %q, want %q", r.path, got[i], r.operation)
				}
			}
		}
		if !slices.Equal(got, reference) {
			t.Fatalf("round %d selected %v, round 0 selected %v", round, got, reference)
		}
	}
}

// A policy with managed operations and no grant compiles, and admits nothing.
func TestDenyAllPolicyCompiles(t *testing.T) {
	p := threeAPIPolicy()
	for i := range p.Operations {
		p.Operations[i].Access = "deny"
	}
	p.Permits = nil
	g, err := Compile(p, 1, time.Time{})
	if err != nil {
		t.Fatalf("deny-all policy rejected: %v", err)
	}
	for _, op := range p.Operations {
		path := op.Target
		if prefix, ok := strings.CutSuffix(op.Target, "*"); ok {
			path = prefix + "x"
		}
		if _, r := g.match(path, op.Method); r == nil || r.permitted {
			t.Errorf("%s %s: %+v, want a matched, denied operation", op.Method, path, r)
		}
	}

	// Without permits no subject is allowed anything, whatever its roles.
	for _, op := range p.Operations {
		_, r := g.match(strings.Replace(op.Target, "*", "x", 1), op.Method)
		for _, subject := range []string{"cust-001", "staff-01", "both@example.test", "nobody-01", "former-01"} {
			if roles, _, reason := g.authorise(testRoles(subject), r); reason == "" {
				t.Errorf("%s allowed %s with roles %v under deny-all", subject, op.ID, roles)
			}
		}
	}

	empty := policy.Policy{SchemaVersion: policy.SchemaVersion, Profile: policy.ProfileCore}
	if _, err := Compile(empty, 1, time.Time{}); err != nil {
		t.Fatalf("empty policy rejected: %v", err)
	}
}

// Roles are flat: a subject is allowed an operation exactly when one of its
// own trusted roles holds a permit for that operation.
func TestRoleEvaluation(t *testing.T) {
	g, err := Compile(threeAPIPolicy(), 1, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		subject, method, path string
		reason                string
		roles                 []string
	}{
		{"staff-01", "GET", "/orders", "", []string{"STAFF"}},
		{"cust-001", "POST", "/orders", "", []string{"CUSTOMER"}},
		{"cust-001", "GET", "/orders", "no_permit", nil},
		{"staff-01", "POST", "/orders", "no_permit", nil},
		// Two roles: each operation is allowed through the one role that holds
		// its permit, and the subject's full role set is reported.
		{"both@example.test", "GET", "/orders", "", []string{"CUSTOMER", "STAFF"}},
		{"both@example.test", "POST", "/orders", "", []string{"CUSTOMER", "STAFF"}},
		{"both@example.test", "GET", "/orders/1", "no_permit", nil},
		{"nobody-01", "GET", "/orders", "no_permit", nil},
		{"former-01", "GET", "/orders", "", []string{"STAFF"}}, // blocking is independent of policy role evaluation
		{"ghost-01", "GET", "/orders", "no_permit", nil},
		{"STAFF", "GET", "/orders", "no_permit", nil},
		{"Staff-01", "GET", "/orders", "no_permit", nil},
		// An operation with no permit at all, and a denied one.
		{"staff-01", "GET", "/inventory/items/1", "no_permit", nil},
		{"staff-01", "GET", "/reports/summary", "no_permit", nil},
	}
	for _, tc := range cases {
		_, r := g.match(tc.path, tc.method)
		if r == nil {
			t.Fatalf("%s %s matched nothing", tc.method, tc.path)
		}
		roles, _, reason := g.authorise(testRoles(tc.subject), r)
		if reason != "" {
			roles = nil
		} // these cases compare grants, not retained audit roles
		if reason != tc.reason || !slices.Equal(roles, tc.roles) {
			t.Errorf("%s %s %s: roles %v reason %q, want %v %q", tc.subject, tc.method, tc.path, roles, reason, tc.roles, tc.reason)
		}
	}
}

// FuzzDecodeAndCompile checks that no input makes the parser or compiler
// panic, and that every accepted input compiles to a stable identity.
func FuzzDecodeAndCompile(f *testing.F) {
	valid, _ := json.Marshal(threeAPIPolicy())
	f.Add(valid)
	f.Add([]byte(`{"schema_version":4,"profile":"core"}`))
	f.Add([]byte(`{"schema_version":4,"profile":"core","profile":"x"}`))
	f.Add([]byte(`{"schema_version":4,"profile":"core","roles":[{"id":"R"}],"subjects":[{"id":"s","status":"active","roles":["R","R"]}]}`))
	f.Add([]byte(`[[[[[[[[[[]]]]]]]]]]`))
	f.Fuzz(func(t *testing.T, document []byte) {
		p, err := policy.DecodePolicy(document)
		if err != nil {
			if _, ok := errors.AsType[*policy.PolicyError](err); !ok {
				t.Fatalf("untyped error %v", err)
			}
			return
		}
		first, err := Compile(p, 1, time.Time{})
		if err != nil {
			if _, ok := errors.AsType[*policy.PolicyError](err); !ok {
				t.Fatalf("untyped error %v", err)
			}
			return
		}
		second, _ := Compile(p, 1, time.Time{})
		if second == nil || first.ID != second.ID {
			t.Fatal("accepted policy compiled to different identities")
		}
	})
}
