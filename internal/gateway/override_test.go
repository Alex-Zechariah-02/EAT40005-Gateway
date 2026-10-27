package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests cover temporary overrides: the request grammar and the
// resolver on their own, then creation, inspection, revocation, expiry and
// rate counters through the real gateway, where every enforcement claim is
// checked at the receivers.

func permitOverride(kind, role, operation string, seconds int64) OverrideRequest {
	return OverrideRequest{Type: kind, Control: controlPermit, Operation: operation, Role: new(role), ExpiresInSeconds: seconds}
}

func rateOverride(kind, operation string, count, seconds int64) OverrideRequest {
	return OverrideRequest{Type: kind, Control: controlRate, Operation: operation, Count: new(count), ExpiresInSeconds: seconds}
}

func operationContainment(operation string, seconds int64) OverrideRequest {
	return OverrideRequest{Type: overrideContainment, Control: controlOperation, Operation: operation, ExpiresInSeconds: seconds}
}

// createOverride asks for a temporary override; request may be an
// OverrideRequest or raw JSON.
func (e *env) createOverride(s adminSession, request any, ifMatch, key string) (int, Outcome, string) {
	e.t.Helper()
	body, ok := request.(string)
	if !ok {
		body = string(mustJSON(request))
	}
	code, data, _ := e.as(s, http.MethodPost, "/admin/overrides", body, "If-Match", ifMatch, "Idempotency-Key", key)
	var outcome Outcome
	json.Unmarshal([]byte(data), &outcome)
	return code, outcome, data
}

// mustOverride creates an override, failing the test unless it is created.
func (e *env) mustOverride(s adminSession, request OverrideRequest, ifMatch, key string) Outcome {
	e.t.Helper()
	code, outcome, body := e.createOverride(s, request, ifMatch, key)
	if code != http.StatusCreated || outcome.Outcome != "committed" || outcome.Override == 0 {
		e.t.Fatalf("creating override %+v: %d %s", request, code, body)
	}
	return outcome
}

func (e *env) revokeOverride(s adminSession, id int64, key string) (int, Outcome, string) {
	e.t.Helper()
	code, data, _ := e.as(s, http.MethodDelete, fmt.Sprintf("/admin/overrides/%d", id), "", "Idempotency-Key", key)
	var outcome Outcome
	json.Unmarshal([]byte(data), &outcome)
	return code, outcome, data
}

type overridesView struct {
	Revision            int64      `json:"revision"`
	Confirmed           int64      `json:"confirmed"`
	Provisional         int64      `json:"provisional"`
	Overrides           []override `json:"overrides"`
	Generation          string     `json:"generation"`
	GenerationOverrides []int64    `json:"generation_overrides"`
}

func (e *env) activeOverrides(s adminSession) overridesView {
	e.t.Helper()
	code, body, _ := e.as(s, http.MethodGet, "/admin/overrides", "")
	if code != http.StatusOK {
		e.t.Fatalf("GET /admin/overrides: %d %s", code, body)
	}
	return fromJSON[overridesView](e.t, body)
}

// storedOverrides reads every override of a stopped gateway.
func storedOverrides(t *testing.T, path string) []override {
	t.Helper()
	db := inspect(t, path)
	defer db.Close()
	s := &store{db: db}
	list, err := s.overrides(t.Context(), `ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	return list
}

// probe sends one request as subject (or anonymously when subject is empty)
// and returns its status.
func (e *env) probe(subject, method, path string) int {
	e.t.Helper()
	var header []string
	if subject != "" {
		header = bearer(e.t, subject)
	}
	body, contentType := "", ""
	if method == http.MethodPost {
		body, contentType = `{"n":1}`, "application/json"
	}
	code, _ := e.request(method, path, contentType, body, header...)
	return code
}

func overrideErrorCode(err error) string {
	if policyErr, ok := errors.AsType[*PolicyError](err); ok {
		return policyErr.Code + "@" + policyErr.Field
	}
	return fmt.Sprint(err)
}

// Only the supported controls and actions can be requested, each with
// exactly the members it takes, well-formed identifiers and a bounded
// lifetime. JWT, validation, logging, upstream or route-allow changes have no
// representation and are refused by name.
func TestOverrideRequestGrammar(t *testing.T) {
	valid := []OverrideRequest{
		permitOverride(overrideException, "READER", "write-a", 60),
		permitOverride(overrideContainment, "WRITER", "read-a", 60),
		rateOverride(overrideException, "read-a", 10, 60),
		rateOverride(overrideContainment, "read-a", 1, 60),
		operationContainment("read-b", 60),
		permitOverride(overrideException, "READER", "write-a", minOverrideSeconds),
		permitOverride(overrideException, "READER", "write-a", maxOverrideSeconds),
	}
	wantTargets := []string{"permit:READER:write-a", "permit:WRITER:read-a", "rate:read-a", "rate:read-a", "operation:read-b",
		"permit:READER:write-a", "permit:READER:write-a"}
	for i, r := range valid {
		o, err := r.check()
		if err != nil || o.Target != wantTargets[i] || o.Status != "" {
			t.Errorf("valid request %d %+v: %+v %v", i, r, o, err)
		}
	}

	withRole := func(r OverrideRequest, role string) OverrideRequest { r.Role = new(role); return r }
	withCount := func(r OverrideRequest, count int64) OverrideRequest { r.Count = new(count); return r }
	withSeconds := func(r OverrideRequest, s int64) OverrideRequest { r.ExpiresInSeconds = s; return r }
	permit := permitOverride(overrideException, "READER", "write-a", 60)
	refused := []struct {
		name    string
		request OverrideRequest
		want    string
	}{
		{"JWT control", OverrideRequest{Type: overrideException, Control: "jwt", Operation: "read-a", ExpiresInSeconds: 60}, "UNSUPPORTED_CONTROL@control"},
		{"authentication control", OverrideRequest{Type: overrideException, Control: "auth", Operation: "read-a", ExpiresInSeconds: 60}, "UNSUPPORTED_CONTROL@control"},
		{"validation control", OverrideRequest{Type: overrideException, Control: "validation", Operation: "write-a", ExpiresInSeconds: 60}, "UNSUPPORTED_CONTROL@control"},
		{"schema control", OverrideRequest{Type: overrideContainment, Control: "schema", Operation: "write-a", ExpiresInSeconds: 60}, "UNSUPPORTED_CONTROL@control"},
		{"logging control", OverrideRequest{Type: overrideException, Control: "logging", Operation: "read-a", ExpiresInSeconds: 60}, "UNSUPPORTED_CONTROL@control"},
		{"audit control", OverrideRequest{Type: overrideContainment, Control: "audit", Operation: "read-a", ExpiresInSeconds: 60}, "UNSUPPORTED_CONTROL@control"},
		{"upstream control", OverrideRequest{Type: overrideException, Control: "upstream", Operation: "read-a", ExpiresInSeconds: 60}, "UNSUPPORTED_CONTROL@control"},
		{"route control name", OverrideRequest{Type: overrideContainment, Control: "route", Operation: "read-a", ExpiresInSeconds: 60}, "UNSUPPORTED_CONTROL@control"},
		{"case variant control", OverrideRequest{Type: overrideException, Control: "Permit", Operation: "write-a", Role: new("READER"), ExpiresInSeconds: 60}, "UNSUPPORTED_CONTROL@control"},
		{"operation allow", OverrideRequest{Type: overrideException, Control: controlOperation, Operation: "denied", ExpiresInSeconds: 60}, "UNSUPPORTED_ACTION@type"},
		{"missing type", OverrideRequest{Control: controlPermit, Operation: "write-a", Role: new("READER"), ExpiresInSeconds: 60}, "INVALID_VALUE@type"},
		{"unknown type", OverrideRequest{Type: "priority", Control: controlPermit, Operation: "write-a", Role: new("READER"), ExpiresInSeconds: 60}, "INVALID_VALUE@type"},
		{"missing control", OverrideRequest{Type: overrideException, Operation: "write-a", ExpiresInSeconds: 60}, "UNSUPPORTED_CONTROL@control"},
		{"missing operation", OverrideRequest{Type: overrideException, Control: controlPermit, Role: new("READER"), ExpiresInSeconds: 60}, "INVALID_VALUE@operation"},
		{"path as operation", OverrideRequest{Type: overrideException, Control: controlPermit, Operation: "/a/*", Role: new("READER"), ExpiresInSeconds: 60}, "INVALID_VALUE@operation"},
		{"method and path as operation", OverrideRequest{Type: overrideException, Control: controlPermit, Operation: "POST /a/item", Role: new("READER"), ExpiresInSeconds: 60}, "INVALID_VALUE@operation"},
		{"encoded operation", OverrideRequest{Type: overrideException, Control: controlPermit, Operation: "write%2Da", Role: new("READER"), ExpiresInSeconds: 60}, "INVALID_VALUE@operation"},
		{"oversized operation", OverrideRequest{Type: overrideContainment, Control: controlOperation, Operation: strings.Repeat("a", maxIDLength+1), ExpiresInSeconds: 60}, "INVALID_VALUE@operation"},
		{"permit without role", OverrideRequest{Type: overrideException, Control: controlPermit, Operation: "write-a", ExpiresInSeconds: 60}, "INVALID_VALUE@role"},
		{"empty role", withRole(permit, ""), "INVALID_VALUE@role"},
		{"role with a line break", withRole(permit, "READER\r\nX"), "INVALID_VALUE@role"},
		{"role on a rate override", withRole(rateOverride(overrideException, "read-a", 10, 60), "READER"), "INVALID_VALUE@role"},
		{"role on an operation containment", withRole(operationContainment("read-a", 60), "READER"), "INVALID_VALUE@role"},
		{"count on a permit override", withCount(permit, 5), "INVALID_VALUE@count"},
		{"rate without count", OverrideRequest{Type: overrideException, Control: controlRate, Operation: "read-a", ExpiresInSeconds: 60}, "INVALID_VALUE@count"},
		{"zero count", rateOverride(overrideContainment, "read-a", 0, 60), "INVALID_VALUE@count"},
		{"negative count", rateOverride(overrideContainment, "read-a", -1, 60), "INVALID_VALUE@count"},
		{"unlimited count", rateOverride(overrideException, "read-a", maxRateCount+1, 60), "INVALID_VALUE@count"},
		{"missing expiry", withSeconds(permit, 0), "INVALID_EXPIRY@expires_in_seconds"},
		{"negative expiry", withSeconds(permit, -60), "INVALID_EXPIRY@expires_in_seconds"},
		{"expiry above a day", withSeconds(permit, maxOverrideSeconds+1), "INVALID_EXPIRY@expires_in_seconds"},
		{"expiry past the duration range", withSeconds(permit, 10_000_000_000), "INVALID_EXPIRY@expires_in_seconds"},
	}
	for _, r := range refused {
		if _, err := r.request.check(); overrideErrorCode(err) != r.want {
			t.Errorf("%s: %v, want %s", r.name, err, r.want)
		}
	}
}

// The resolver applies each supported change to a copy of the base and
// nothing else, refuses a change that no longer does what its type says, and
// produces the same generation whatever order the overrides come in.
func TestApplyOverrides(t *testing.T) {
	e := &env{a: &receiver{name: "A", addr: "127.0.0.1:1"}, b: &receiver{name: "B", addr: "127.0.0.1:2"}}
	basePolicy := e.protectedPolicy()
	basePolicy.Operations[0].Rate = rate(4, 60) // read-a
	pristine := string(mustJSON(basePolicy))

	stored := func(id int64, r OverrideRequest) override {
		o, err := r.check()
		if err != nil {
			t.Fatalf("request %+v: %v", r, err)
		}
		o.ID, o.ExpiresAt = id, time.UnixMilli(time.Now().Add(time.Hour).UnixMilli())
		return o
	}
	all := []override{
		stored(1, permitOverride(overrideException, "READER", "write-a", 60)),
		stored(2, permitOverride(overrideContainment, "WRITER", "read-a", 60)),
		stored(3, rateOverride(overrideException, "read-a", 9, 60)),
		stored(4, operationContainment("read-b", 60)),
		stored(5, permitOverride(overrideContainment, "READER", "read-a", 60)),
	}
	p, err := applyOverrides(basePolicy, all)
	if err != nil {
		t.Fatal(err)
	}
	if string(mustJSON(basePolicy)) != pristine {
		t.Fatal("applying overrides changed the base policy")
	}
	wantPermits := []Permit{{Role: "WRITER", Operation: "write-a"}, {Role: "READER", Operation: "read-b"}, {Role: "READER", Operation: "write-a"}}
	if !slices.Equal(p.Permits, wantPermits) {
		t.Fatalf("permits %v, want %v", p.Permits, wantPermits)
	}
	readA, _ := findOperation(p, "read-a")
	readB, _ := findOperation(p, "read-b")
	if readA.Rate.Count != 9 || readA.Rate.WindowSeconds != 60 || readA.Rate.Key != "subject" || readA.Rate.Algorithm != "sliding-window" ||
		readA.Access != "allow" || readB.Access != "deny" {
		t.Fatalf("operations after overrides: read-a %+v rate %+v, read-b %+v", readA, readA.Rate, readB)
	}
	// Apart from the permits, the rate count and the access flag, the
	// result is the base: same routes, methods, destinations, authentication,
	// schemas, roles and subjects.
	restored := p
	restored.Permits = basePolicy.Permits
	restored.Operations = slices.Clone(p.Operations)
	restored.Operations[0].Rate = basePolicy.Operations[0].Rate
	for i := range restored.Operations {
		if restored.Operations[i].ID == "read-b" {
			restored.Operations[i].Access = "allow"
		}
	}
	if string(mustJSON(restored)) != pristine {
		t.Fatalf("overrides changed more than their targets:\n%s\n%s", mustJSON(restored), pristine)
	}

	// The same overrides in any order give one generation; none gives the
	// base's own generation identity.
	deadline := time.Time{}
	first, err := effectiveGeneration(basePolicy, 7, deadline, all)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(first.ID, "r7+o1.2.3.4.5-") || !slices.Equal(first.Overrides, []int64{1, 2, 3, 4, 5}) {
		t.Fatalf("generation %s overrides %v", first.ID, first.Overrides)
	}
	for range 50 {
		shuffled := slices.Clone(all)
		rand.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		again, err := effectiveGeneration(basePolicy, 7, deadline, shuffled)
		if err != nil || again.ID != first.ID {
			t.Fatalf("order %v gave %v %v, want %s", overrideIDs(shuffled), again, err, first.ID)
		}
	}
	plain, err := Compile(basePolicy, 7, deadline)
	if err != nil {
		t.Fatal(err)
	}
	none, err := effectiveGeneration(basePolicy, 7, deadline, nil)
	if err != nil || none.ID != plain.ID || len(none.Overrides) != 0 {
		t.Fatalf("no overrides: %v %v, want %s", none, err, plain.ID)
	}
	// The generation expires at the earliest expiry of its overrides.
	soon := slices.Clone(all)
	soon[2].ExpiresAt = time.UnixMilli(time.Now().Add(time.Minute).UnixMilli())
	g, err := effectiveGeneration(basePolicy, 7, time.UnixMilli(time.Now().Add(2*time.Minute).UnixMilli()), soon)
	if err != nil || !g.NotAfter.Equal(soon[2].ExpiresAt) {
		t.Fatalf("NotAfter %v, want %v (%v)", g.NotAfter, soon[2].ExpiresAt, err)
	}

	// Each refusal names what no longer fits; nothing is partly applied.
	refused := []struct {
		name string
		list []override
		want string
	}{
		{"grant of a permit the base has", []override{stored(1, permitOverride(overrideException, "WRITER", "write-a", 60))}, "INVALID_VALUE@role"},
		{"removal of a permit the base lacks", []override{stored(1, permitOverride(overrideContainment, "READER", "write-a", 60))}, "INVALID_VALUE@role"},
		{"permit on a public operation", []override{stored(1, permitOverride(overrideException, "READER", "denied", 60))}, "INVALID_VALUE@operation"},
		{"unknown role", []override{stored(1, permitOverride(overrideException, "ADMIN", "write-a", 60))}, "UNKNOWN_REFERENCE@role"},
		{"unknown operation", []override{stored(1, operationContainment("delete-a", 60))}, "UNKNOWN_REFERENCE@operation"},
		{"rate exception that does not raise", []override{stored(1, rateOverride(overrideException, "read-a", 4, 60))}, "INVALID_VALUE@count"},
		{"rate containment that does not lower", []override{stored(1, rateOverride(overrideContainment, "read-a", 5, 60))}, "INVALID_VALUE@count"},
		{"rate override without a base rule", []override{stored(1, rateOverride(overrideException, "read-b", 10, 60))}, "INVALID_VALUE@operation"},
		{"containment of a denied operation", []override{stored(1, operationContainment("denied", 60))}, "INVALID_VALUE@operation"},
		{"two overrides of one target", []override{stored(1, rateOverride(overrideException, "read-a", 9, 60)), stored(2, rateOverride(overrideContainment, "read-a", 1, 60))}, "OVERRIDE_CONFLICT@rate:read-a"},
	}
	for _, r := range refused {
		if _, err := applyOverrides(basePolicy, r.list); overrideErrorCode(err) != r.want {
			t.Errorf("%s: %v, want %s", r.name, err, r.want)
		}
		if g, err := effectiveGeneration(basePolicy, 7, deadline, r.list); g != nil || err == nil {
			t.Errorf("%s: a generation was compiled", r.name)
		}
	}
	// A permit exception on an operation the base denies cannot open it.
	denied := basePolicy
	denied.Operations = slices.Clone(basePolicy.Operations)
	denied.Operations[1].Access = "deny" // write-a
	if _, err := applyOverrides(denied, []override{stored(1, permitOverride(overrideException, "READER", "write-a", 60))}); overrideErrorCode(err) != "INVALID_VALUE@operation" {
		t.Errorf("permit exception on a denied operation: %v", err)
	}
	tooMany := make([]override, maxActiveOverrides+1)
	if _, err := effectiveGeneration(basePolicy, 7, deadline, tooMany); overrideErrorCode(err) != "LIMIT_EXCEEDED@overrides" {
		t.Errorf("too many overrides: %v", err)
	}
}

// A candidate conflicts with an active override when it changes the
// override's own target, the operation it applies to or, for a permit, the
// role, or when it adds, moves or removes a more specific operation inside
// the override's path prefix. Anything else may proceed with the override in
// place.
func TestOverrideConflictWithCandidate(t *testing.T) {
	e := &env{a: &receiver{name: "A", addr: "127.0.0.1:1"}, b: &receiver{name: "B", addr: "127.0.0.1:2"}}
	before := e.protectedPolicy()
	before.Operations[0].Rate = rate(4, 60)
	overrides := []override{
		{ID: 3, Type: overrideException, Control: controlPermit, Role: "READER", Operation: "write-a", Target: "permit:READER:write-a"},
		{ID: 4, Type: overrideContainment, Control: controlOperation, Operation: "read-b", Target: "operation:read-b"},
		{ID: 5, Type: overrideException, Control: controlRate, Operation: "read-a", Count: 9, Target: "rate:read-a"},
	}
	change := func(edit func(p *Policy)) Policy {
		p := before
		p.Operations = slices.Clone(before.Operations)
		p.Permits = slices.Clone(before.Permits)
		p.Roles = slices.Clone(before.Roles)
		edit(&p)
		return p
	}
	cases := []struct {
		name string
		edit func(p *Policy)
		want int64 // conflicting override, 0 for none
	}{
		{"same permit granted permanently", func(p *Policy) { p.Permits = append(p.Permits, Permit{Role: "READER", Operation: "write-a"}) }, 3},
		{"role of a permit override removed", func(p *Policy) {
			p.Roles = p.Roles[1:] // READER
			p.Permits = slices.DeleteFunc(p.Permits, func(x Permit) bool { return x.Role == "READER" })
		}, 3},
		{"operation of a permit override changed", func(p *Policy) { p.Operations[1].Body = jsonBody(512, anyObject) }, 3},
		{"operation of a permit override removed", func(p *Policy) {
			p.Operations = slices.Delete(p.Operations, 1, 2)
			p.Permits = slices.DeleteFunc(p.Permits, func(x Permit) bool { return x.Operation == "write-a" })
		}, 3},
		{"rate count changed", func(p *Policy) { p.Operations[0].Rate = rate(6, 60) }, 5},
		{"rate rule removed", func(p *Policy) { p.Operations[0].Rate = nil }, 5},
		{"rate window changed", func(p *Policy) { p.Operations[0].Rate = rate(4, 120) }, 5},
		{"contained operation re-routed", func(p *Policy) { p.Operations[3].Target = "/c/*" }, 4},
		{"contained operation denied permanently", func(p *Policy) { p.Operations[3].Access = "deny" }, 4},
		{"more specific operation added inside a contained prefix", func(p *Policy) {
			p.Operations = append(p.Operations, Operation{ID: "special-b", API: "b", Method: "GET", Target: "/b/special", Access: "allow", Auth: "none"})
		}, 4},
		{"more specific prefix added inside an overridden prefix, other method", func(p *Policy) {
			p.Operations = append(p.Operations, Operation{ID: "deep-a", API: "a", Method: "DELETE", Target: "/a/deep/*", Access: "allow", Auth: "none"})
		}, 5},
		{"unrelated permit added", func(p *Policy) { p.Permits = append(p.Permits, Permit{Role: "WRITER", Operation: "read-b"}) }, 0},
		{"unrelated permit removed", func(p *Policy) {
			p.Permits = slices.DeleteFunc(p.Permits, func(x Permit) bool { return x == Permit{Role: "WRITER", Operation: "read-a"} })
		}, 0},
		{"unrelated operation re-routed outside every prefix", func(p *Policy) { p.Operations[2].Target = "/dd" }, 0},
		{"rate rule added to an overridden operation, an independent control", func(p *Policy) { p.Operations[1].Rate = rate(9, 60) }, 0},
		{"less specific operation added above an overridden prefix", func(p *Policy) {
			p.Operations = append(p.Operations, Operation{ID: "all", API: "d", Method: "GET", Target: "/*", Access: "allow", Auth: "none"})
		}, 0},
		{"operation added beside a prefix, outside it", func(p *Policy) {
			p.Operations = append(p.Operations, Operation{ID: "bee", API: "b", Method: "GET", Target: "/bee", Access: "allow", Auth: "none"})
		}, 0},
		{"unrelated role added", func(p *Policy) { p.Roles = append(p.Roles, Role{ID: "OTHER"}) }, 0},
		{"nothing changed", func(p *Policy) {}, 0},
	}
	for _, c := range cases {
		got := overrideConflict(before, change(c.edit), overrides)
		switch {
		case c.want == 0 && got != nil:
			t.Errorf("%s: conflicts with override %d", c.name, got.ID)
		case c.want != 0 && (got == nil || got.ID != c.want):
			t.Errorf("%s: %v, want override %d", c.name, got, c.want)
		}
	}
	if narrows("/a/item", "/a/item/x") || !narrows("/a/*", "/a/x") || narrows("/a/*", "/a/") || narrows("/a/*", "/ab") || !narrows("/*", "/a") {
		t.Error("narrows does not follow most-specific matching")
	}
}

// Each supported override changes enforcement at once, exactly as its type
// says, without touching the confirmed base; JWT authentication still
// applies to a granted permit. Overrides survive an unrelated permanent
// change, and revoking one removes that one only.
func TestOverrideLifecycle(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.plain)
	harness := e.mustLogin(harnessAdmin, harnessPassword)
	baseGeneration := e.status().Generation

	// The base: bob (READER) reads /a and /b but may not write /a; alice
	// (WRITER) reads and writes /a.
	expect := func(want map[string]int, receipts [3]int) {
		t.Helper()
		e.resetReceivers()
		got := map[string]int{
			"bob POST /a/item":   e.probe("bob", http.MethodPost, "/a/item"),
			"bob GET /a/x":       e.probe("bob", http.MethodGet, "/a/x"),
			"bob GET /b/x":       e.probe("bob", http.MethodGet, "/b/x"),
			"alice GET /a/x":     e.probe("alice", http.MethodGet, "/a/x"),
			"alice POST /a/item": e.probe("alice", http.MethodPost, "/a/item"),
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("enforcement %v, want %v", got, want)
		}
		e.assertReceipts(receipts[0], receipts[1], receipts[2])
	}
	expect(map[string]int{"bob POST /a/item": 403, "bob GET /a/x": 200, "bob GET /b/x": 200, "alice GET /a/x": 200, "alice POST /a/item": 200}, [3]int{3, 1, 0})

	// An RBAC exception grants bob the write.
	before := time.Now()
	grant := e.mustOverride(harness, permitOverride(overrideException, "READER", "write-a", 600), `"r1"`, "lifecycle-grant-0001")
	if grant.Override != 1 || grant.Target != "permit:READER:write-a" || grant.Revision != 1 || !strings.HasPrefix(grant.Generation, "r1+o1-") ||
		grant.Expires.Before(before.Add(600*time.Second).Add(-time.Millisecond)) || grant.Expires.After(time.Now().Add(600*time.Second)) {
		t.Fatalf("grant receipt %+v", grant)
	}
	expect(map[string]int{"bob POST /a/item": 200, "bob GET /a/x": 200, "bob GET /b/x": 200, "alice GET /a/x": 200, "alice POST /a/item": 200}, [3]int{4, 1, 0})
	for _, r := range e.a.receipts() {
		if r.Generation != grant.Generation {
			t.Fatalf("A received generation %s, want %s", r.Generation, grant.Generation)
		}
	}
	// The granted permit still needs a valid token of a known subject.
	e.resetReceivers()
	for _, header := range [][]string{nil, {"Authorization", "Bearer x.y.z"}, {"Authorization", "Bearer " + issuer.tokenFor(t, "mallory")},
		{"Authorization", "Bearer " + hmacToken(issuer.publicPEM(), claimsJSON(validClaims("bob")))}} {
		code, _ := e.request(http.MethodPost, "/a/item", "application/json", `{"n":1}`, append(header, "X-Gateway-Roles", "READER,WRITER")...)
		if code != http.StatusUnauthorized && code != http.StatusForbidden {
			t.Fatalf("POST /a/item with %v: %d", header, code)
		}
	}
	// A body the schema refuses is still refused.
	if code, _ := e.request(http.MethodPost, "/a/item", "application/json", `[1]`, bearer(t, "bob")...); code != http.StatusBadRequest {
		t.Fatalf("invalid body under the grant: %d", code)
	}
	e.assertReceipts(0, 0, 0)

	// An RBAC containment withdraws alice's read; a route containment closes
	// /b for everyone.
	contain := e.mustOverride(harness, permitOverride(overrideContainment, "WRITER", "read-a", 600), `"r1"`, "lifecycle-contain-01")
	closed := e.mustOverride(harness, operationContainment("read-b", 600), `"r1"`, "lifecycle-close-0001")
	if !strings.HasPrefix(closed.Generation, "r1+o1.2.3-") {
		t.Fatalf("generation %s", closed.Generation)
	}
	expect(map[string]int{"bob POST /a/item": 200, "bob GET /a/x": 200, "bob GET /b/x": 403, "alice GET /a/x": 403, "alice POST /a/item": 200}, [3]int{3, 0, 0})

	// Inspection shows the active set and the generation serving it; the
	// confirmed base is still revision 1.
	view := e.activeOverrides(harness)
	if view.Revision != 1 || view.Confirmed != 1 || view.Generation != closed.Generation || !slices.Equal(view.GenerationOverrides, []int64{1, 2, 3}) ||
		len(view.Overrides) != 3 || view.Overrides[1].Target != "permit:WRITER:read-a" || view.Overrides[1].Status != overrideActive ||
		view.Overrides[1].Actor != harnessAdmin || view.Overrides[1].Base != 1 || view.Overrides[1].Creation != contain.Transition {
		t.Fatalf("overrides view %+v", view)
	}
	if code, body, header := e.as(harness, http.MethodGet, "/admin/policy", ""); code != http.StatusOK || header.Get("ETag") != `"r1"` ||
		strings.Contains(body, `"role":"READER","operation":"write-a"`) {
		t.Fatalf("confirmed policy: %d %s", code, header.Get("ETag"))
	}
	if s := e.status(); s.Revision != 1 || s.Confirmed != 1 || s.Generation != closed.Generation {
		t.Fatalf("status %+v", s)
	}

	// An unrelated permanent change keeps every override.
	next := e.protectedPolicy()
	next.Roles = append(next.Roles, Role{ID: "TRIAL"})
	next.Permits = append(next.Permits, Permit{Role: "TRIAL", Operation: "read-a"})
	e.confirmPolicy(harness, next, 1, "lifecycle-permanent")
	s := e.status()
	if s.Confirmed != 2 || !strings.HasPrefix(s.Generation, "r2+o1.2.3-") {
		t.Fatalf("status after the membership change %+v", s)
	}
	expect(map[string]int{"bob POST /a/item": 200, "bob GET /a/x": 200, "bob GET /b/x": 403, "alice GET /a/x": 403, "alice POST /a/item": 200}, [3]int{3, 0, 0})
	if code := e.probe("carol", http.MethodGet, "/a/x"); code != http.StatusOK {
		t.Fatalf("new TRIAL permit applies independently of WRITER containment: %d", code)
	}

	// Revoking the grant removes it alone, over the current base (with
	// carol), never an earlier policy.
	code, revoked, body := e.revokeOverride(harness, grant.Override, "lifecycle-revoke-01")
	if code != http.StatusOK || revoked.Disposition != overrideRevoked || revoked.Override != grant.Override || revoked.Revision != 2 ||
		!strings.HasPrefix(revoked.Generation, "r2+o2.3-") {
		t.Fatalf("revocation: %d %s", code, body)
	}
	expect(map[string]int{"bob POST /a/item": 403, "bob GET /a/x": 200, "bob GET /b/x": 403, "alice GET /a/x": 403, "alice POST /a/item": 200}, [3]int{2, 0, 0})
	if code := e.probe("carol", http.MethodGet, "/a/x"); code != http.StatusOK {
		t.Fatalf("carol after the revocation: %d", code)
	}
	// Retries and stale revocations.
	if code, again, body := e.revokeOverride(harness, grant.Override, "lifecycle-revoke-01"); code != http.StatusOK || again.Transition != revoked.Transition ||
		again.Disposition != overrideRevoked || again.Override != grant.Override {
		t.Fatalf("retried revocation: %d %s", code, body)
	}
	if code, again, body := e.revokeOverride(harness, grant.Override, "lifecycle-revoke-02"); code != http.StatusConflict || again.Reason != "ALREADY_RESOLVED" ||
		again.Disposition != overrideRevoked {
		t.Fatalf("second revocation: %d %s", code, body)
	}
	if code, _, body := e.revokeOverride(harness, 99, "lifecycle-revoke-03"); code != http.StatusNotFound {
		t.Fatalf("unknown override: %d %s", code, body)
	}
	// The identical creation request returns its recorded receipt and creates
	// nothing, although the base has moved on; changed in any way (here its
	// precondition), it is refused.
	if code, again, body := e.createOverride(harness, permitOverride(overrideException, "READER", "write-a", 600), `"r1"`, "lifecycle-grant-0001"); code != http.StatusCreated ||
		again.Transition != grant.Transition || again.Override != grant.Override {
		t.Fatalf("replayed creation: %d %s", code, body)
	}
	if code, again, body := e.createOverride(harness, permitOverride(overrideException, "READER", "write-a", 600), `"r2"`, "lifecycle-grant-0001"); code != http.StatusUnprocessableEntity ||
		again.Reason != "TRANSITION_ID_REUSED" {
		t.Fatalf("creation key reused for another request: %d %s", code, body)
	}
	if n := len(e.activeOverrides(harness).Overrides); n != 2 {
		t.Fatalf("%d active overrides after the replay, want 2", n)
	}
	// Revoke the rest: the base is enforced again with the membership change.
	for i, id := range []int64{contain.Override, closed.Override} {
		if code, _, body := e.revokeOverride(harness, id, fmt.Sprintf("lifecycle-revoke-rest-%d", i)); code != http.StatusOK {
			t.Fatalf("revocation of %d: %d %s", id, code, body)
		}
	}
	s = e.status()
	if s.Confirmed != 2 || !strings.HasPrefix(s.Generation, "r2-") {
		t.Fatalf("status without overrides %+v", s)
	}
	expect(map[string]int{"bob POST /a/item": 403, "bob GET /a/x": 200, "bob GET /b/x": 200, "alice GET /a/x": 200, "alice POST /a/item": 200}, [3]int{3, 1, 0})
	if s.Generation == baseGeneration {
		t.Fatal("the generation of revision 2 equals that of revision 1")
	}

	// History, events and durable state.
	code, body, _ = e.as(harness, http.MethodGet, "/admin/overrides/history", "")
	history := fromJSON[struct {
		Items []override `json:"items"`
	}](t, body)
	if code != http.StatusOK || len(history.Items) != 3 {
		t.Fatalf("override history: %d %s", code, body)
	}
	for _, o := range history.Items {
		if o.Status != overrideRevoked || o.ResolvedBy != harnessAdmin || o.ResolvedAt == nil || o.Resolution == "" {
			t.Fatalf("history item %+v", o)
		}
	}
	if code, body, _ := e.as(harness, http.MethodGet, "/admin/overrides/2", ""); code != http.StatusOK || !strings.Contains(body, `"target":"permit:WRITER:read-a"`) {
		t.Fatalf("one override: %d %s", code, body)
	}
	events := e.eventsWhen(func(events []map[string]any) bool { return countKind(events, "tpo.revoked") >= 3 })
	if countKind(events, "tpo.created") != 3 || countKind(events, "tpo.revoked") != 3 {
		t.Fatalf("lifecycle events: %d created, %d revoked", countKind(events, "tpo.created"), countKind(events, "tpo.revoked"))
	}
	created := findTransition(events, "tpo.created", grant.Transition)
	revokedEvent := findTransition(events, "tpo.revoked", revoked.Transition)
	if created == nil || created["override"] != float64(1) || created["target"] != "permit:READER:write-a" || created["actor"] != harnessAdmin ||
		created["generation"] != grant.Generation || created["revision"] != float64(1) || created["expires_at"] == nil ||
		revokedEvent == nil || revokedEvent["generation"] != revoked.Generation || revokedEvent["override"] != float64(1) {
		t.Fatalf("events: created %v revoked %v", created, revokedEvent)
	}
	assertAbsent(t, e, harness.cookie, harness.csrf, harnessPassword)
	e.stop()
	if n := queryInt(t, e.database, `SELECT COUNT(*) FROM revision`); n != 2 {
		t.Fatalf("%d revisions: overrides created revisions", n)
	}
	if n := queryInt(t, e.database, `SELECT COUNT(*) FROM transition WHERE kind LIKE 'tpo.%' AND outcome = 'committed' AND published_at IS NOT NULL AND audited_at IS NOT NULL`); n != 6 {
		t.Fatalf("%d published and audited override transitions, want 6", n)
	}
	for _, o := range storedOverrides(t, e.database) {
		if o.Status != overrideRevoked || o.Base != 1 {
			t.Fatalf("stored override %+v", o)
		}
	}
}

// A refused or malformed override request changes nothing: no row, no
// generation, no receipt. Refusals with an identity are recorded once;
// malformed requests are answered 400 without reaching the controller.
func TestOverrideRefusals(t *testing.T) {
	e := newEnv(t)
	p := e.protectedPolicy()
	p.Operations[0].Rate = rate(4, 60) // read-a
	seed(t, e.database, p, seedOptions{})
	e.start(binaries.plain)
	_, readerPassword := e.addAdmin(binaries.plain, "reader", "read,read_history")
	harness := e.mustLogin(harnessAdmin, harnessPassword)
	reader := e.mustLogin("reader", readerPassword)
	before := e.status()
	apiToken := issuer.tokenFor(t, "alice")

	refusals := []struct {
		name    string
		request any
		ifMatch string
		status  int
		reason  string
		field   string
	}{
		{"JWT bypass", `{"type":"exception","control":"jwt","operation":"read-a","expires_in_seconds":60}`, `"r1"`, 400, "UNSUPPORTED_CONTROL", "control"},
		{"validation bypass", `{"type":"exception","control":"validation","operation":"write-a","expires_in_seconds":60}`, `"r1"`, 400, "UNSUPPORTED_CONTROL", "control"},
		{"logging bypass", `{"type":"containment","control":"logging","operation":"read-a","expires_in_seconds":60}`, `"r1"`, 400, "UNSUPPORTED_CONTROL", "control"},
		{"upstream change", `{"type":"exception","control":"upstream","operation":"read-a","expires_in_seconds":60}`, `"r1"`, 400, "UNSUPPORTED_CONTROL", "control"},
		{"route allow", operationContainment("denied", 60), `"r1"`, 400, "INVALID_VALUE", "operation"},
		{"route exception", `{"type":"exception","control":"operation","operation":"denied","expires_in_seconds":60}`, `"r1"`, 400, "UNSUPPORTED_ACTION", "type"},
		{"unknown operation", operationContainment("delete-everything", 60), `"r1"`, 400, "UNKNOWN_REFERENCE", "operation"},
		{"unknown role", permitOverride(overrideException, "ADMIN", "write-a", 60), `"r1"`, 400, "UNKNOWN_REFERENCE", "role"},
		{"token as role", permitOverride(overrideException, apiToken, "write-a", 60), `"r1"`, 400, "INVALID_VALUE", "role"},
		{"record injection in a role", permitOverride(overrideException, "READER\r\n{\"kind\":\"tpo.created\",\"forged\":true}", "write-a", 60), `"r1"`, 400, "INVALID_VALUE", "role"},
		{"record injection in an operation", operationContainment("read-b\n{\"forged\":true}", 60), `"r1"`, 400, "INVALID_VALUE", "operation"},
		{"already granted", permitOverride(overrideException, "WRITER", "write-a", 60), `"r1"`, 400, "INVALID_VALUE", "role"},
		{"rate exception lowering", rateOverride(overrideException, "read-a", 2, 60), `"r1"`, 400, "INVALID_VALUE", "count"},
		{"rate on an unrated operation", rateOverride(overrideContainment, "write-a", 1, 60), `"r1"`, 400, "INVALID_VALUE", "operation"},
		{"unlimited rate", rateOverride(overrideException, "read-a", maxRateCount+1, 60), `"r1"`, 400, "INVALID_VALUE", "count"},
		{"no expiry", `{"type":"exception","control":"permit","operation":"write-a","role":"READER"}`, `"r1"`, 400, "INVALID_EXPIRY", "expires_in_seconds"},
		{"expired already", permitOverride(overrideException, "READER", "write-a", -1), `"r1"`, 400, "INVALID_EXPIRY", "expires_in_seconds"},
		{"expiry above a day", permitOverride(overrideException, "READER", "write-a", 86401), `"r1"`, 400, "INVALID_EXPIRY", "expires_in_seconds"},
		{"stale precondition", permitOverride(overrideException, "READER", "write-a", 60), `"r0"`, 412, "PRECONDITION_FAILED", ""},
	}
	for i, r := range refusals {
		code, outcome, body := e.createOverride(harness, r.request, r.ifMatch, fmt.Sprintf("refusal-%02d-xxxxxxxx", i))
		if code != r.status || outcome.Outcome != "rejected" || outcome.Reason != r.reason || outcome.Field != r.field {
			t.Errorf("%s: %d %s, want %d %s %s", r.name, code, body, r.status, r.reason, r.field)
		}
	}
	// Malformed requests, including fields that would change the key, window
	// or algorithm of a rate rule or a destination: refused before the
	// controller, nothing recorded.
	malformed := []string{
		`{"type":"exception","control":"rate","operation":"read-a","count":9,"window_seconds":1,"expires_in_seconds":60}`,
		`{"type":"exception","control":"rate","operation":"read-a","count":9,"key":"client_ip","expires_in_seconds":60}`,
		`{"type":"exception","control":"rate","operation":"read-a","count":9,"algorithm":"none","expires_in_seconds":60}`,
		`{"type":"exception","control":"permit","operation":"write-a","role":"READER","upstream":"10.0.0.1:80","expires_in_seconds":60}`,
		`{"type":"exception","control":"permit","operation":"write-a","role":"READER","expires_in_seconds":60,"priority":1}`,
		`{"type":"exception","control":"permit","operation":"write-a","role":"READER","role":"WRITER","expires_in_seconds":60}`,
		`{"Type":"exception","control":"permit","operation":"write-a","role":"READER","expires_in_seconds":60}`,
		`{"type":"exception","control":"rate","operation":"read-a","count":9.5,"expires_in_seconds":60}`,
		`{"type":"exception","control":"permit","operation":"write-a","role":"READER","expires_in_seconds":"60"}`,
		`{"type":"exception","control":"permit","operation":"write-a","role":"READER","expires_in_seconds":1e30}`,
		`{"type":"exception","control":"permit","operation":"write-a","role":"READER","expires_in_seconds":60}}`,
		`[]`,
	}
	for i, body := range malformed {
		if code, _, data := e.createOverride(harness, body, `"r1"`, fmt.Sprintf("malformed-%02d-xxxxxxx", i)); code != http.StatusBadRequest || !strings.Contains(data, "bad_request") {
			t.Errorf("malformed %s: %d %s", body, code, data)
		}
	}
	good := string(mustJSON(permitOverride(overrideException, "READER", "write-a", 60)))
	if code, _, _ := e.as(harness, http.MethodPost, "/admin/overrides", good, "Idempotency-Key", "no-if-match-0001"); code != http.StatusPreconditionRequired {
		t.Errorf("without If-Match: %d", code)
	}
	if code, _, _ := e.as(harness, http.MethodPost, "/admin/overrides", good, "If-Match", `"r1"`); code != http.StatusBadRequest {
		t.Errorf("without Idempotency-Key: %d", code)
	}
	if code, _, _ := e.as(harness, http.MethodPut, "/admin/overrides/1", good); code != http.StatusMethodNotAllowed {
		t.Errorf("PUT of an override: %d", code)
	}
	if code, _, _ := e.as(harness, http.MethodPatch, "/admin/overrides/1", good); code != http.StatusMethodNotAllowed {
		t.Errorf("PATCH of an override: %d", code)
	}

	// Callers without the right credential or permission.
	type attempt struct {
		name   string
		header []string
		status int
	}
	attempts := []attempt{
		{"no session", []string{"Origin", e.adminOrigin()}, http.StatusUnauthorized},
		{"API client token", []string{"Origin", e.adminOrigin(), "Authorization", "Bearer " + apiToken}, http.StatusUnauthorized},
		{"forged session cookie", []string{"Cookie", sessionCookie + "=" + strings.Repeat("A", 43), "Origin", e.adminOrigin()}, http.StatusUnauthorized},
		{"read-only administrator", reader.headers(e), http.StatusForbidden},
		{"no CSRF token", []string{"Cookie", sessionCookie + "=" + harness.cookie, "Origin", e.adminOrigin()}, http.StatusForbidden},
		{"another origin", []string{"Cookie", sessionCookie + "=" + harness.cookie, csrfHeader, harness.csrf, "Origin", "http://evil.example"}, http.StatusForbidden},
		{"another host", append(harness.headers(e), "Host", "localhost"), http.StatusBadRequest},
	}
	for i, a := range attempts {
		for _, call := range []struct{ method, path, body string }{
			{http.MethodPost, "/admin/overrides", good},
			{http.MethodPost, "/admin/overrides/validate", good},
			{http.MethodDelete, "/admin/overrides/1", ""},
		} {
			header := append(slices.Clone(a.header), "If-Match", `"r1"`, "Idempotency-Key", fmt.Sprintf("unauthorised-%02d-xxxx", i))
			if code, body, _ := e.adminSend(call.method, call.path, call.body, header...); code != a.status {
				t.Errorf("%s %s, %s: %d %s, want %d", call.method, call.path, a.name, code, body, a.status)
			}
		}
	}
	if code, _, _ := e.adminSend(http.MethodGet, "/admin/overrides", ""); code != http.StatusUnauthorized {
		t.Errorf("anonymous overrides view: %d", code)
	}
	if code, _, _ := e.as(reader, http.MethodGet, "/admin/overrides", ""); code != http.StatusOK {
		t.Errorf("reader overrides view: %d", code)
	}
	// The native Caddy administration endpoint does not exist.
	if dialable("127.0.0.1:2019") {
		t.Error("something listens on the native Caddy administration port")
	}

	// Validation answers without creating anything.
	code, body, _ := e.as(harness, http.MethodPost, "/admin/overrides/validate", good, "If-Match", `"r1"`)
	if valid := fromJSON[Outcome](t, body); code != http.StatusOK || valid.Outcome != "valid" || valid.Target != "permit:READER:write-a" || valid.Expires.IsZero() {
		t.Fatalf("validation: %d %s", code, body)
	}
	bad := string(mustJSON(permitOverride(overrideException, "ADMIN", "write-a", 60)))
	if code, body, _ := e.as(harness, http.MethodPost, "/admin/overrides/validate", bad, "If-Match", `"r1"`); code != http.StatusBadRequest ||
		!strings.Contains(body, "UNKNOWN_REFERENCE") {
		t.Fatalf("validation of an unknown role: %d %s", code, body)
	}

	// The longest lifetime is accepted. The same key with another body is
	// refused; the first is kept.
	before24h := time.Now()
	first := e.mustOverride(harness, permitOverride(overrideException, "READER", "write-a", maxOverrideSeconds), `"r1"`, "reused-key-000001")
	if first.Expires.Before(before24h.Add(24*time.Hour).Add(-time.Millisecond)) || first.Expires.After(time.Now().Add(24*time.Hour)) {
		t.Fatalf("expiry %v is not a day after the creation", first.Expires)
	}
	if code, outcome, body := e.createOverride(harness, permitOverride(overrideException, "READER", "write-a", 120), `"r1"`, "reused-key-000001"); code != http.StatusUnprocessableEntity ||
		outcome.Reason != "TRANSITION_ID_REUSED" {
		t.Fatalf("reused key: %d %s", code, body)
	}
	if code, _, body := e.revokeOverride(harness, first.Override, "revoke-refusals-01"); code != http.StatusOK {
		t.Fatalf("revocation: %d %s", code, body)
	}

	// Nothing else changed; only the one created override was ever stored.
	after := e.status()
	if after.Revision != before.Revision || after.Confirmed != 1 || !strings.HasPrefix(after.Generation, "r1-") || after.Generation != before.Generation {
		t.Fatalf("status after the refusals %+v, before %+v", after, before)
	}
	e.resetReceivers()
	if e.probe("bob", http.MethodPost, "/a/item") != http.StatusForbidden || e.probe("bob", http.MethodGet, "/b/x") != http.StatusOK {
		t.Fatal("enforcement changed")
	}
	e.assertReceipts(0, 1, 0)
	events := e.eventsWhen(func(events []map[string]any) bool { return countKind(events, "tpo.rejected") >= len(refusals)+1 })
	if n := countKind(events, "tpo.rejected"); n < len(refusals)+1 {
		t.Fatalf("%d refusal events", n)
	}
	if countKind(events, "tpo.validated") != 1 || countKind(events, "tpo.created") != 1 {
		t.Fatal("validation or creation events missing")
	}
	assertAbsent(t, e, apiToken, harness.cookie, harness.csrf, reader.cookie, `"forged"`)
	e.stop()
	if list := storedOverrides(t, e.database); len(list) != 1 {
		t.Fatalf("stored overrides %+v", list)
	}
	if n := queryInt(t, e.database, `SELECT COUNT(*) FROM transition WHERE kind = 'tpo.create' AND outcome = 'rejected'`); n != len(refusals) {
		t.Fatalf("%d recorded refusals, want %d", n, len(refusals))
	}
	if n := queryInt(t, e.database, `SELECT COUNT(*) FROM transition WHERE id LIKE '%unauthorised%' OR id LIKE '%malformed%'`); n != 0 {
		t.Fatalf("%d unauthorised or malformed requests reached the history", n)
	}
}

// At most one active override governs a control and target: a second one is
// refused naming the first, also when created concurrently, and the database
// refuses it too. Overrides of different targets coexist, including several
// controls of one operation and permits of different roles on it. Targets
// are policy identifiers, so another spelling is simply unknown.
func TestOverrideConflicts(t *testing.T) {
	e := newEnv(t)
	p := e.protectedPolicy()
	p.Operations[0].Rate = rate(4, 60) // read-a
	seed(t, e.database, p, seedOptions{})
	e.start(binaries.plain)
	harness := e.mustLogin(harnessAdmin, harnessPassword)

	first := e.mustOverride(harness, rateOverride(overrideException, "read-a", 8, 600), `"r1"`, "conflict-first-001")
	refused := []struct {
		name    string
		request OverrideRequest
		reason  string
	}{
		{"same target", rateOverride(overrideException, "read-a", 8, 600), "OVERRIDE_CONFLICT"},
		{"same target, other action", rateOverride(overrideContainment, "read-a", 1, 60), "OVERRIDE_CONFLICT"},
		{"same target, other count and expiry", rateOverride(overrideException, "read-a", 100, 30), "OVERRIDE_CONFLICT"},
		{"case variant", rateOverride(overrideException, "Read-a", 8, 600), "UNKNOWN_REFERENCE"},
		{"path instead of identifier", rateOverride(overrideException, "/a/*", 8, 600), "INVALID_VALUE"},
		{"trailing space", rateOverride(overrideException, "read-a ", 8, 600), "INVALID_VALUE"},
	}
	for i, r := range refused {
		code, outcome, body := e.createOverride(harness, r.request, `"r1"`, fmt.Sprintf("conflict-refused-%02d", i))
		if outcome.Reason != r.reason || (r.reason == "OVERRIDE_CONFLICT" && (code != http.StatusConflict || outcome.Override != first.Override)) {
			t.Errorf("%s: %d %s", r.name, code, body)
		}
	}

	// Independent targets on the same operations coexist.
	independent := []OverrideRequest{
		permitOverride(overrideContainment, "READER", "read-a", 600),
		permitOverride(overrideContainment, "WRITER", "read-a", 600),
		operationContainment("read-a", 600),
		permitOverride(overrideException, "READER", "write-a", 600),
	}
	var last Outcome
	for i, r := range independent {
		last = e.mustOverride(harness, r, `"r1"`, fmt.Sprintf("conflict-independent-%d", i))
	}
	if !strings.HasPrefix(last.Generation, "r1+o1.") || len(e.activeOverrides(harness).Overrides) != 5 {
		t.Fatalf("independent overrides: %+v", last)
	}
	// With read-a contained, every read of /a is refused before
	// authentication and reaches nothing; bob's granted write works.
	e.resetReceivers()
	if e.probe("bob", http.MethodGet, "/a/x") != http.StatusForbidden || e.probe("", http.MethodGet, "/a/x") != http.StatusForbidden ||
		e.probe("bob", http.MethodPost, "/a/item") != http.StatusOK {
		t.Fatal("independent overrides not all enforced")
	}
	e.assertReceipts(1, 0, 0)

	// Eight concurrent creations of one new target: exactly one succeeds.
	var wg sync.WaitGroup
	start := make(chan struct{})
	var mu sync.Mutex
	codes := map[int]int{}
	for i := range 8 {
		wg.Go(func() {
			<-start
			code, outcome, _ := e.createOverride(harness, operationContainment("read-b", 600), `"r1"`, fmt.Sprintf("conflict-race-%02d-xx", i))
			mu.Lock()
			codes[code]++
			if code == http.StatusConflict && outcome.Reason != "OVERRIDE_CONFLICT" {
				codes[-1]++
			}
			mu.Unlock()
		})
	}
	close(start)
	wg.Wait()
	if codes[http.StatusCreated] != 1 || codes[http.StatusConflict] != 7 || codes[-1] != 0 {
		t.Fatalf("concurrent creations answered %v", codes)
	}

	e.stop()
	list := storedOverrides(t, e.database)
	active := map[string]int{}
	for _, o := range list {
		if o.Status == overrideActive {
			active[o.Target]++
		}
	}
	// The first, four independent ones and one race winner.
	if len(list) != 6 || len(active) != 6 {
		t.Fatalf("stored overrides %+v", list)
	}
	for target, n := range active {
		if n != 1 {
			t.Fatalf("%d active overrides of %s", n, target)
		}
	}
	// The database itself refuses a second active override of a target.
	db := inspect(t, e.database)
	_, err := db.Exec(`INSERT INTO override (id, type, control, operation, count, target, base, created_at, expires_at, actor, creation, status)
		VALUES (100, 'containment', 'rate', 'read-a', 1, 'rate:read-a', 1, 1, 2, 'x', ?, 'active')`, list[0].Creation)
	if err == nil {
		t.Fatal("the database accepted a second active override of rate:read-a")
	}
	// It also refuses a target that does not match the override.
	_, err = db.Exec(`INSERT INTO override (id, type, control, operation, count, target, base, created_at, expires_at, actor, creation, status)
		VALUES (101, 'containment', 'rate', 'read-b', 1, 'rate:read-a', 1, 1, 2, 'x', ?, 'revoked')`, list[1].Creation)
	if err == nil {
		t.Fatal("the database accepted an override whose target names another operation")
	}
	db.Close()
}

// An override expires at its expiry time without any request or
// administrator action; the base then applies again. A revocation decided at
// or after the expiry time finds the override expired, and a revocation
// racing the expiry produces one final disposition.
func TestOverrideExpiry(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.plain)
	harness := e.mustLogin(harnessAdmin, harnessPassword)
	baseGeneration := e.status().Generation

	// Expiry with no traffic: only status reads while the time passes.
	kept := e.mustOverride(harness, operationContainment("read-b", 600), `"r1"`, "expiry-kept-000001")
	expiring := e.mustOverride(harness, permitOverride(overrideException, "READER", "write-a", 2), `"r1"`, "expiry-quiet-00001")
	waitFor(t, expiring.Expires.Add(5*time.Second), "the expiry", func() bool { return len(e.status().Overrides) == 1 })
	s := e.status()
	if !strings.HasPrefix(s.Generation, fmt.Sprintf("r1+o%d-", kept.Override)) {
		t.Fatalf("status after the expiry %+v", s)
	}
	e.resetReceivers()
	if e.probe("bob", http.MethodPost, "/a/item") != http.StatusForbidden || e.probe("bob", http.MethodGet, "/b/x") != http.StatusForbidden {
		t.Fatal("after the expiry, the base and the remaining override are not what is enforced")
	}
	e.assertReceipts(0, 0, 0)
	code, outcome, body := e.revokeOverride(harness, expiring.Override, "expiry-late-revoke")
	if code != http.StatusConflict || outcome.Reason != "ALREADY_RESOLVED" || outcome.Disposition != overrideExpired {
		t.Fatalf("revocation after the expiry: %d %s", code, body)
	}
	if code, _, body := e.revokeOverride(harness, kept.Override, "expiry-kept-revoke"); code != http.StatusOK {
		t.Fatalf("revocation: %d %s", code, body)
	}
	if s := e.status(); s.Generation != baseGeneration {
		t.Fatalf("generation %s after all overrides ended, base generation %s", s.Generation, baseGeneration)
	}

	// Revocations racing the expiry, several rounds.
	for round := range 4 {
		o := e.mustOverride(harness, permitOverride(overrideException, "READER", "write-a", 1), `"r1"`, fmt.Sprintf("expiry-race-%02d-xxx", round))
		time.Sleep(time.Until(o.Expires.Add(time.Duration(round*5-10) * time.Millisecond)))
		code, outcome, body := e.revokeOverride(harness, o.Override, fmt.Sprintf("expiry-race-%02d-rev", round))
		switch {
		case code == http.StatusOK && outcome.Disposition == overrideRevoked:
		case code == http.StatusConflict && outcome.Disposition == overrideExpired:
		default:
			t.Fatalf("round %d: %d %s", round, code, body)
		}
	}
	waitFor(t, time.Now().Add(5*time.Second), "every override to end", func() bool { return len(e.status().Overrides) == 0 })

	events := e.eventsWhen(func(events []map[string]any) bool { return countKind(events, "tpo.expired") >= 1 })
	ev := findTransition(events, "tpo.expired", fmt.Sprintf("auto-expire:%d", expiring.Override))
	if ev == nil || ev["actor"] != "controller" || ev["override"] != float64(expiring.Override) || ev["revision"] != float64(1) {
		t.Fatalf("expiry event %v", ev)
	}
	e.stop()
	for _, o := range storedOverrides(t, e.database) {
		resolution := fmt.Sprintf("auto-expire:%d", o.ID)
		switch {
		case o.Status == overrideExpired && o.ResolvedBy == "controller" && o.Resolution == resolution:
			// Expired no earlier than its expiry time, and promptly after it.
			if o.ResolvedAt.Before(o.ExpiresAt) || o.ResolvedAt.Sub(o.ExpiresAt) > time.Second {
				t.Fatalf("override %d expired %v after its expiry time", o.ID, o.ResolvedAt.Sub(o.ExpiresAt))
			}
		case o.Status == overrideRevoked && o.ResolvedBy == harnessAdmin:
			if !o.ResolvedAt.Before(o.ExpiresAt) {
				t.Fatalf("override %d revoked at or after its expiry time", o.ID)
			}
		default:
			t.Fatalf("override %+v has no single valid disposition", o)
		}
		n := queryInt(t, e.database, `SELECT COUNT(*) FROM transition WHERE outcome = 'committed' AND (kind = 'tpo.revoke' OR kind = 'tpo.expire')
			AND id = ?`, o.Resolution)
		if n != 1 {
			t.Fatalf("override %d has %d ending transitions", o.ID, n)
		}
	}
	if n := queryInt(t, e.database, `SELECT COUNT(*) FROM transition WHERE outcome = 'committed' AND kind IN ('tpo.revoke', 'tpo.expire')`); n != 6 {
		t.Fatalf("%d ending transitions for 6 overrides", n)
	}
}

// With the expiry timer disabled (fault build), no request sent at or after
// an override's expiry time is admitted under it: the first request that
// sees the expired generation is refused and makes the controller end the
// override, once, however many see it at once. A revocation decided just
// before the expiry time revokes; one decided at or after it finds the
// override expired.
func TestExpiredOverrideAdmitsNothing(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.fault, "GATEWAY_FAULT=no-deadline-timer")
	harness := e.mustLogin(harnessAdmin, harnessPassword)

	o := e.mustOverride(harness, permitOverride(overrideException, "READER", "write-a", 2), `"r1"`, "late-timer-traffic")
	bobToken := "Bearer " + issuer.tokenFor(t, "bob")
	e.resetReceivers()
	var admittedBefore, refusedAfter, refusedStraddling, baseAfter int
	for time.Now().Before(o.Expires.Add(1500 * time.Millisecond)) {
		sent := time.Now()
		code, _ := e.request(http.MethodPost, "/a/item", "application/json", `{}`, "Authorization", bobToken)
		atOrAfter := !sent.Before(o.Expires)
		switch {
		case code == http.StatusOK && atOrAfter:
			t.Fatalf("write sent %v after the expiry was admitted", sent.Sub(o.Expires))
		case code == http.StatusOK:
			admittedBefore++
		case code == http.StatusServiceUnavailable && atOrAfter:
			refusedAfter++
		case code == http.StatusServiceUnavailable && o.Expires.Sub(sent) < 200*time.Millisecond:
			// Sent just before the expiry but captured at or after it.
			refusedStraddling++
		case code == http.StatusForbidden && atOrAfter:
			baseAfter++
		default:
			t.Fatalf("write sent %v from the expiry: %d", sent.Sub(o.Expires), code)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The request that first sees the expired generation is refused and wakes
	// the controller; it may have been sent just before the expiry time, and
	// then every later request already sees the base.
	if admittedBefore == 0 || refusedAfter+refusedStraddling == 0 || baseAfter == 0 {
		t.Fatalf("admitted before %d, refused after %d (+%d sent just before), base after %d", admittedBefore, refusedAfter, refusedStraddling, baseAfter)
	}
	if n := e.a.count(); n != admittedBefore {
		t.Fatalf("A received %d writes, %d admitted", n, admittedBefore)
	}

	// Many requests see the expiry at once; one expiry happens.
	crowded := e.mustOverride(harness, permitOverride(overrideException, "READER", "write-a", 1), `"r1"`, "late-timer-crowded")
	time.Sleep(time.Until(crowded.Expires.Add(50 * time.Millisecond)))
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 20 {
		wg.Go(func() {
			<-start
			e.get("/b/x", "Authorization", bobToken)
		})
	}
	close(start)
	wg.Wait()
	waitFor(t, time.Now().Add(5*time.Second), "the expiry after concurrent requests", func() bool { return len(e.status().Overrides) == 0 })

	// Revocations at the decision instant around the expiry time.
	for i, offset := range []time.Duration{-40 * time.Millisecond, -10 * time.Millisecond, 0, 10 * time.Millisecond, 40 * time.Millisecond} {
		o := e.mustOverride(harness, permitOverride(overrideException, "READER", "write-a", 1), `"r1"`, fmt.Sprintf("boundary-create-%02d", i))
		time.Sleep(time.Until(o.Expires.Add(offset)))
		sent := time.Now()
		code, outcome, body := e.revokeOverride(harness, o.Override, fmt.Sprintf("boundary-revoke-%02d", i))
		switch {
		case code == http.StatusOK && outcome.Disposition == overrideRevoked:
			if !sent.Before(o.Expires) {
				t.Fatalf("revocation sent %v after the expiry was committed", sent.Sub(o.Expires))
			}
		case code == http.StatusConflict && outcome.Disposition == overrideExpired:
		default:
			t.Fatalf("offset %v: %d %s", offset, code, body)
		}
		t.Logf("revocation sent %v from the expiry: %d %s", sent.Sub(o.Expires), code, outcome.Disposition)
	}

	e.stop()
	for _, o := range storedOverrides(t, e.database) {
		switch o.Status {
		case overrideExpired:
			if o.ResolvedAt.Before(o.ExpiresAt) || o.Resolution != fmt.Sprintf("auto-expire:%d", o.ID) {
				t.Fatalf("expired override %+v", o)
			}
		case overrideRevoked:
			if !o.ResolvedAt.Before(o.ExpiresAt) {
				t.Fatalf("revoked override %+v decided at or after its expiry", o)
			}
		default:
			t.Fatalf("override %+v", o)
		}
	}
}

// Rate overrides change only the count of an existing rule. The counters
// already accrued keep applying across creation, revocation and expiry, so
// no change grants a fresh quota, and subjects keep separate counters. The
// expected decisions are worked out by hand from the admission rule
// (admit when C + P*(W-d)/W + 1 <= N), not taken from the implementation.
func TestRateOverrideKeepsCounters(t *testing.T) {
	ratePolicy := func(e *env) Policy {
		p := e.protectedPolicy()
		p.Roles = append(p.Roles, Role{ID: "TRIAL"})
		p.Permits = append(p.Permits, Permit{Role: "TRIAL", Operation: "read-a"})
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
	raise := e.mustOverride(harness, rateOverride(overrideException, "read-a", 6, 600), `"r1"`, "rate-raise-000001")
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
	contain := e.mustOverride(harness, rateOverride(overrideContainment, "read-a", 2, 600), `"r1"`, "rate-lower-000001")
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
	race := e.mustOverride(harness, rateOverride(overrideException, "read-a", 5, 600), `"r1"`, "rate-race-0000001")
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
	expiring := e.mustOverride(harness, rateOverride(overrideException, "read-a", 6, 2), `"r1"`, "rate-expiring-0001")
	next := ratePolicy(e)
	next.Roles = append(next.Roles, Role{ID: "OTHER"})
	e.confirmPolicy(harness, next, 1, "rate-permanent-change")
	if got := e.sendAs("alice", http.MethodGet, "/a/x", "", 5); !statusesAre(got, times(5, 200)...) {
		t.Fatalf("alice under the expiring exception: %v", got)
	}
	waitFor(t, expiring.Expires.Add(5*time.Second), "the rate exception to expire", func() bool { return len(e.status().Overrides) == 0 })
	// The base count 4 applies to the 5 already counted: no fresh quota.
	if got := e.sendAs("alice", http.MethodGet, "/a/x", "", 1); !statusesAre(got, 429) {
		t.Fatalf("alice after the expiry: %v", got)
	}
	// The unrelated change survived the expiry: dave is a member, with his
	// own counter under the base count.
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
