// Policy decoding, determinism and limits.

package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

// These tests exercise the parser and compiler directly. They are pure: no
// process, database or network is involved. Publication of the same inputs
// through the real controller is covered by the publication tests.

// threeAPIPolicy is a complete policy for three APIs with role and permit
// declarations, written independently of any test expectation.
func threeAPIPolicy() policy.Policy {
	return policy.Policy{
		SchemaVersion: policy.SchemaVersion,
		Profile:       policy.ProfileCore,
		Upstreams: []policy.Upstream{
			{ID: "orders", Dial: "127.0.0.1:9101"},
			{ID: "inventory", Dial: "127.0.0.1:9102"},
			{ID: "reports", Dial: "[::1]:9103"},
		},
		APIs: []policy.API{
			{ID: "orders", Upstream: "orders"},
			{ID: "inventory", Upstream: "inventory"},
			{ID: "reports", Upstream: "reports"},
		},
		Operations: []policy.Operation{
			{ID: "orders.list", API: "orders", Method: "GET", Target: "/orders", Access: "allow", Auth: "required"},
			{ID: "orders.get", API: "orders", Method: "GET", Target: "/orders/*", Access: "allow", Auth: "required"},
			{ID: "orders.create", API: "orders", Method: "POST", Target: "/orders", Access: "allow", Auth: "required", Body: jsonBody(1024, anyObject)},
			{ID: "inventory.health", API: "inventory", Method: "GET", Target: "/inventory/health", Access: "allow", Auth: "none"},
			{ID: "inventory.get", API: "inventory", Method: "GET", Target: "/inventory/items/*", Access: "allow", Auth: "required"},
			{ID: "reports.summary", API: "reports", Method: "GET", Target: "/reports/summary", Access: "deny", Auth: "required"},
		},
		Roles: []policy.Role{{ID: "CUSTOMER"}, {ID: "STAFF"}},
		Permits: []policy.Permit{
			{Role: "STAFF", Operation: "orders.list"},
			{Role: "CUSTOMER", Operation: "orders.create"},
		},
	}
}

func compileJSON(t *testing.T, document string) error {
	t.Helper()
	p, err := policy.DecodePolicy([]byte(document))
	if err != nil {
		return err
	}
	_, err = Compile(p, 1, time.Time{})
	return err
}

func errorCode(err error) string {
	if policyErr, ok := errors.AsType[*policy.PolicyError](err); ok {
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
	decoded, err := policy.DecodePolicy(original)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Compile(decoded, 7, time.Time{})
	if err != nil || second.ID != first.ID || first.Profile != policy.ProfileCore {
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
		{"nesting above the bound", head + `,"roles":` + strings.Repeat("[", policy.MaxPolicyDepth) + "1" + strings.Repeat("]", policy.MaxPolicyDepth) + `}`, "LIMIT_EXCEEDED"},
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
		{"too many upstreams", head + `,"upstreams":[` + repeat(policy.MaxUpstreams+1, func(i int) string {
			return fmt.Sprintf(`{"id":"u%d","dial":"127.0.0.1:%d"}`, i, 1000+i)
		}) + `]}`},
		{"too many roles", head + `,"roles":[` + repeat(policy.MaxRoles+1, func(i int) string { return fmt.Sprintf(`{"id":"r%d"}`, i) }) + `]}`},
		{"too many operations", head + `,"operations":[` + repeat(policy.MaxOperations+1, func(i int) string {
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
	p.Roles = append(p.Roles, policy.Role{ID: strings.Repeat("R", policy.MaxIDLength+1)})
	if got := errorCode(compileErr(p)); got != "INVALID_VALUE" {
		t.Errorf("overlong identifier: %q", got)
	}
	p = threeAPIPolicy()
	p.Operations[0].Target = "/" + strings.Repeat("a", policy.MaxTargetLength)
	if got := errorCode(compileErr(p)); got != "INVALID_TARGET" {
		t.Errorf("overlong target: %q", got)
	}
	p = threeAPIPolicy()
	p.Operations[2].Body.MaxBytes = policy.MaxBodyLimit + 1
	if got := errorCode(compileErr(p)); got != "INVALID_VALUE" {
		t.Errorf("body limit above the maximum: %q", got)
	}
}

func compileErr(p policy.Policy) error {
	_, err := Compile(p, 1, time.Time{})
	return err
}
