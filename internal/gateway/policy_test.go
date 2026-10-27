package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// These tests exercise the parser and compiler directly. They are pure: no
// process, database or network is involved. Publication of the same inputs
// through the real controller is covered by the publication tests.

// threeAPIPolicy is a complete policy for three APIs with role and permit
// declarations, written independently of any test expectation.
func threeAPIPolicy() Policy {
	return Policy{
		SchemaVersion: schemaVersion,
		Profile:       profileCore,
		Upstreams: []Upstream{
			{ID: "orders", Dial: "127.0.0.1:9101"},
			{ID: "inventory", Dial: "127.0.0.1:9102"},
			{ID: "reports", Dial: "[::1]:9103"},
		},
		APIs: []API{
			{ID: "orders", Upstream: "orders"},
			{ID: "inventory", Upstream: "inventory"},
			{ID: "reports", Upstream: "reports"},
		},
		Operations: []Operation{
			{ID: "orders.list", API: "orders", Method: "GET", Target: "/orders", Access: "allow", Auth: "required"},
			{ID: "orders.get", API: "orders", Method: "GET", Target: "/orders/*", Access: "allow", Auth: "required"},
			{ID: "orders.create", API: "orders", Method: "POST", Target: "/orders", Access: "allow", Auth: "required", Body: jsonBody(1024, anyObject)},
			{ID: "inventory.health", API: "inventory", Method: "GET", Target: "/inventory/health", Access: "allow", Auth: "none"},
			{ID: "inventory.get", API: "inventory", Method: "GET", Target: "/inventory/items/*", Access: "allow", Auth: "required"},
			{ID: "reports.summary", API: "reports", Method: "GET", Target: "/reports/summary", Access: "deny", Auth: "required"},
		},
		Roles: []Role{{ID: "CUSTOMER"}, {ID: "STAFF"}},
		Permits: []Permit{
			{Role: "STAFF", Operation: "orders.list"},
			{Role: "CUSTOMER", Operation: "orders.create"},
		},
	}
}

func compileJSON(t *testing.T, document string) error {
	t.Helper()
	p, err := DecodePolicy([]byte(document))
	if err != nil {
		return err
	}
	_, err = Compile(p, 1, time.Time{})
	return err
}

func errorCode(err error) string {
	if policyErr, ok := errors.AsType[*PolicyError](err); ok {
		return policyErr.Code
	}
	if err == nil {
		return ""
	}
	return "untyped: " + err.Error()
}

func TestValidPolicyCompilesDeterministically(t *testing.T) {
	p := threeAPIPolicy()
	original, _ := json.Marshal(p)

	first, err := Compile(p, 7, time.Time{})
	if err != nil {
		t.Fatalf("valid policy rejected: %v", err)
	}
	if after, _ := json.Marshal(p); string(after) != string(original) {
		t.Fatal("compiling changed its input")
	}

	// The same document, decoded from JSON, compiles to the same identity.
	decoded, err := DecodePolicy(original)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Compile(decoded, 7, time.Time{})
	if err != nil || second.ID != first.ID || first.Profile != profileCore {
		t.Fatalf("recompile: %v, ids %s and %s", err, first.ID, second.ID)
	}

	// The generation shares nothing mutable with the policy it came from.
	p.Operations[0].Access = "deny"
	p.Upstreams[0].Dial = "127.0.0.1:1"
	p.Roles[1].ID = "OTHER"
	p.Permits[0].Role = "CUSTOMER"
	_, r := first.match("/orders", "GET")
	if r == nil || !r.permitted || r.dial != "127.0.0.1:9101" || r.api != "orders" || r.upstream != "orders" {
		t.Fatalf("generation changed with its source policy: %+v", r)
	}
	if _, _, reason := first.authorise([]string{"STAFF"}, r); reason != "" {
		t.Fatalf("permit changed with its source policy: %s", reason)
	}
	if _, ignored, reason := first.authorise([]string{"OTHER"}, r); reason != "no_permit" || !slices.Equal(ignored, []string{"OTHER"}) {
		t.Fatalf("declared roles changed with source: %s %v", reason, ignored)
	}

	// A different revision is a different generation.
	other, _ := Compile(threeAPIPolicy(), 8, time.Time{})
	if other.ID == first.ID {
		t.Fatal("revision is not part of the generation identity")
	}
}

func TestMalformedPolicyDocumentsAreRejected(t *testing.T) {
	valid, _ := json.Marshal(threeAPIPolicy())
	head := `{"schema_version":4,"profile":"core"`
	cases := []struct {
		name     string
		document string
		code     string
	}{
		{"malformed JSON", `{"schema_version":4,`, "SCHEMA_INVALID"},
		{"truncated document", string(valid[:len(valid)/2]), "SCHEMA_INVALID"},
		{"trailing data", string(valid) + "}", "SCHEMA_INVALID"},
		{"not an object", `[1]`, "SCHEMA_INVALID"},
		{"duplicate member", head + `,"profile":"core"}`, "DUPLICATE_MEMBER"},
		{"duplicate nested member", head + `,"upstreams":[{"id":"a","id":"b","dial":"127.0.0.1:1"}]}`, "DUPLICATE_MEMBER"},
		{"duplicate subject member", head + `,"subjects":[{"id":"a","status":"active","roles":[],"roles":["STAFF"]}]}`, "DUPLICATE_MEMBER"},
		{"case variant of a field", head + `,"Profile":"core"}`, "UNKNOWN_FIELD"},
		{"case variant of a nested field", head + `,"upstreams":[{"ID":"a","dial":"127.0.0.1:1"}]}`, "UNKNOWN_FIELD"},
		{"unknown field", head + `,"rate_rules":[]}`, "UNKNOWN_FIELD"},
		{"schema reference not supported", head + `,"schemas":[]}`, "UNKNOWN_FIELD"},
		{"unknown nested field", head + `,"operations":[{"id":"x","api":"a","method":"GET","target":"/x","access":"allow","auth":"none","rbac":"off"}]}`, "UNKNOWN_FIELD"},
		{"subject with an inherited role field", head + `,"subjects":[{"id":"a","status":"active","roles":[],"inherits":"ADMIN"}]}`, "UNKNOWN_FIELD"},
		{"wrong type", head + `,"upstreams":{}}`, "SCHEMA_INVALID"},
		{"removed subjects even with wrong roles type", head + `,"subjects":[{"id":"a","status":"active","roles":"STAFF"}]}`, "UNKNOWN_FIELD"},
		{"wrong scalar type", `{"schema_version":"2"}`, "SCHEMA_INVALID"},
		{"missing schema version", `{"profile":"core"}`, "SCHEMA_VERSION_UNSUPPORTED"},
		{"earlier schema version", `{"schema_version":1,"profile":"routing-only"}`, "SCHEMA_VERSION_UNSUPPORTED"},
		{"previous schema version", `{"schema_version":2,"profile":"jwt-rbac"}`, "SCHEMA_VERSION_UNSUPPORTED"},
		{"retired schema version", `{"schema_version":3,"profile":"core"}`, "SCHEMA_VERSION_UNSUPPORTED"},
		{"later schema version", `{"schema_version":5,"profile":"core"}`, "SCHEMA_VERSION_UNSUPPORTED"},
		{"missing profile", `{"schema_version":4}`, "INVALID_VALUE"},
		{"routing-only profile", `{"schema_version":4,"profile":"routing-only"}`, "UNSUPPORTED_FEATURE"},
		{"previous profile", `{"schema_version":4,"profile":"jwt-rbac"}`, "UNSUPPORTED_FEATURE"},
		{"profile claiming full enforcement", `{"schema_version":4,"profile":"protected"}`, "UNSUPPORTED_FEATURE"},
		{"missing access", head + `,"upstreams":[{"id":"u","dial":"127.0.0.1:1"}],"apis":[{"id":"a","upstream":"u"}],"operations":[{"id":"x","api":"a","method":"GET","target":"/x","auth":"none"}]}`, "INVALID_VALUE"},
		{"missing auth", head + `,"upstreams":[{"id":"u","dial":"127.0.0.1:1"}],"apis":[{"id":"a","upstream":"u"}],"operations":[{"id":"x","api":"a","method":"GET","target":"/x","access":"allow"}]}`, "INVALID_VALUE"},
		{"oversize document", head + `,"roles":[` + strings.Repeat(`{"id":"r"},`, 7000) + `{"id":"r"}]}`, "LIMIT_EXCEEDED"},
		{"deep nesting", strings.Repeat("[", 5000) + strings.Repeat("]", 5000), "LIMIT_EXCEEDED"},
		{"nesting above the bound", head + `,"roles":` + strings.Repeat("[", maxPolicyDepth) + "1" + strings.Repeat("]", maxPolicyDepth) + `}`, "LIMIT_EXCEEDED"},
	}
	for _, tc := range cases {
		started := time.Now()
		err := compileJSON(t, tc.document)
		if got := errorCode(err); got != tc.code {
			t.Errorf("%s: %q, want %s", tc.name, got, tc.code)
		}
		// Rejection must be bounded; one second is a generous ceiling for
		// inputs of at most 64 KiB.
		if elapsed := time.Since(started); elapsed > time.Second {
			t.Errorf("%s: took %v", tc.name, elapsed)
		}
	}
}

func TestPolicyCountAndLengthLimits(t *testing.T) {
	repeat := func(n int, item func(i int) string) string {
		items := make([]string, n)
		for i := range items {
			items[i] = item(i)
		}
		return strings.Join(items, ",")
	}
	head := `{"schema_version":4,"profile":"core"`
	cases := []struct {
		name     string
		document string
	}{
		{"removed subjects at former count bound", head + `,"subjects":[` + repeat(1025, func(i int) string {
			return fmt.Sprintf(`{"id":"s%d","status":"active","roles":[]}`, i)
		}) + `]}`},
		{"too many upstreams", head + `,"upstreams":[` + repeat(maxUpstreams+1, func(i int) string {
			return fmt.Sprintf(`{"id":"u%d","dial":"127.0.0.1:%d"}`, i, 1000+i)
		}) + `]}`},
		{"too many roles", head + `,"roles":[` + repeat(maxRoles+1, func(i int) string { return fmt.Sprintf(`{"id":"r%d"}`, i) }) + `]}`},
		{"too many operations", head + `,"operations":[` + repeat(maxOperations+1, func(i int) string {
			return fmt.Sprintf(`{"id":"o%d","api":"a","method":"GET","target":"/o%d","access":"allow","auth":"none"}`, i, i)
		}) + `]}`},
	}
	for _, tc := range cases {
		want := "LIMIT_EXCEEDED"
		if tc.name == "removed subjects at former count bound" {
			want = "UNKNOWN_FIELD"
		}
		if got := errorCode(compileJSON(t, tc.document)); got != want {
			t.Errorf("%s: %q, want %s", tc.name, got, want)
		}
	}

	p := threeAPIPolicy()
	p.Roles = append(p.Roles, Role{ID: strings.Repeat("R", maxIDLength+1)})
	if got := errorCode(compileErr(p)); got != "INVALID_VALUE" {
		t.Errorf("overlong identifier: %q", got)
	}
	p = threeAPIPolicy()
	p.Operations[0].Target = "/" + strings.Repeat("a", maxTargetLength)
	if got := errorCode(compileErr(p)); got != "INVALID_TARGET" {
		t.Errorf("overlong target: %q", got)
	}
	p = threeAPIPolicy()
	p.Operations[2].Body.MaxBytes = maxBodyLimit + 1
	if got := errorCode(compileErr(p)); got != "INVALID_VALUE" {
		t.Errorf("body limit above the maximum: %q", got)
	}
}

func compileErr(p Policy) error {
	_, err := Compile(p, 1, time.Time{})
	return err
}

func TestInvalidReferencesAndConflictsAreRejected(t *testing.T) {
	cases := []struct {
		name   string
		change func(p *Policy)
		code   string
		field  string
	}{
		{"unknown API", func(p *Policy) { p.Operations[0].API = "billing" }, "UNKNOWN_REFERENCE", "operations[0].api"},
		{"unknown upstream", func(p *Policy) { p.APIs[1].Upstream = "elsewhere" }, "UNKNOWN_REFERENCE", "apis[1].upstream"},
		{"unknown role", func(p *Policy) { p.Permits[0].Role = "ADMIN" }, "UNKNOWN_REFERENCE", "permits[0].role"},
		{"unknown operation", func(p *Policy) { p.Permits[1].Operation = "orders.delete" }, "UNKNOWN_REFERENCE", "permits[1].operation"},
		{"permit on public operation", func(p *Policy) { p.Permits[0].Operation = "inventory.health" }, "INVALID_VALUE", "permits[0].operation"},
		{"duplicate permit", func(p *Policy) { p.Permits = append(p.Permits, p.Permits[0]) }, "DUPLICATE_ID", "permits[2]"},
		{"duplicate operation id", func(p *Policy) { p.Operations[1].ID = "orders.list" }, "DUPLICATE_ID", "operations[1].id"},
		{"duplicate API id", func(p *Policy) { p.APIs[2].ID = "orders" }, "DUPLICATE_ID", "apis[2].id"},
		{"duplicate upstream id", func(p *Policy) { p.Upstreams[2].ID = "orders" }, "DUPLICATE_ID", "upstreams[2].id"},
		{"duplicate role", func(p *Policy) { p.Roles[1].ID = "CUSTOMER" }, "DUPLICATE_ID", "roles[1].id"},
		{"same target and method twice", func(p *Policy) {
			p.Operations = append(p.Operations, Operation{ID: "orders.list2", API: "inventory", Method: "GET", Target: "/orders", Access: "deny", Auth: "none"})
		}, "DUPLICATE_OPERATION", "operations[6]"},
		{"same prefix and method twice", func(p *Policy) {
			p.Operations = append(p.Operations, Operation{ID: "orders.get2", API: "orders", Method: "GET", Target: "/orders/*", Access: "allow", Auth: "none"})
		}, "DUPLICATE_OPERATION", "operations[6]"},
		{"two upstreams with one destination", func(p *Policy) { p.Upstreams[1].Dial = p.Upstreams[0].Dial }, "INVALID_UPSTREAM", "upstreams[1].dial"},
		{"identifier with a space", func(p *Policy) { p.Operations[0].ID = "orders list" }, "INVALID_VALUE", "operations[0].id"},
		{"identifier with a newline", func(p *Policy) { p.Roles[0].ID = "CUSTOMER\n" }, "INVALID_VALUE", "roles[0].id"},
		{"empty identifier", func(p *Policy) { p.APIs[0].ID = "" }, "INVALID_VALUE", "apis[0].id"},
		{"unknown access value", func(p *Policy) { p.Operations[0].Access = "permit" }, "INVALID_VALUE", "operations[0].access"},
		{"unknown auth value", func(p *Policy) { p.Operations[0].Auth = "optional" }, "INVALID_VALUE", "operations[0].auth"},
		{"zero body limit", func(p *Policy) { p.Operations[2].Body.MaxBytes = 0 }, "INVALID_VALUE", "operations[2].body.max_bytes"},
	}
	for _, tc := range cases {
		p := threeAPIPolicy()
		tc.change(&p)
		err := compileErr(p)
		policyErr, ok := errors.AsType[*PolicyError](err)
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
		{ID: strings.Repeat("s", maxSubjectLength+1), Status: "active"},
	} {
		if _, err := DecodePolicy(withSubject(threeAPIPolicy(), subject)); errorCode(err) != "UNKNOWN_FIELD" {
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
		Operation{ID: "all", API: "reports", Method: "GET", Target: "/*", Access: "allow", Auth: "none"},
		Operation{ID: "items", API: "inventory", Method: "GET", Target: "/inventory/*", Access: "allow", Auth: "none"},
		Operation{ID: "special", API: "inventory", Method: "GET", Target: "/inventory/items/special", Access: "allow", Auth: "none"},
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

	empty := Policy{SchemaVersion: schemaVersion, Profile: profileCore}
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
		p, err := DecodePolicy(document)
		if err != nil {
			if _, ok := errors.AsType[*PolicyError](err); !ok {
				t.Fatalf("untyped error %v", err)
			}
			return
		}
		first, err := Compile(p, 1, time.Time{})
		if err != nil {
			if _, ok := errors.AsType[*PolicyError](err); !ok {
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
