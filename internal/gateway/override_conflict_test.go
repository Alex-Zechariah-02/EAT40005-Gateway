// Override conflicts with candidates and each other.

package gateway

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/Alex-Zechariah-02/apiward/internal/policy"
	"github.com/Alex-Zechariah-02/apiward/internal/store"
)

// A candidate conflicts with an active override when it changes the
// override's own target, the operation it applies to or, for a permit, the
// role, or when it adds, moves or removes a more specific operation inside
// the override's path prefix. Anything else may proceed with the override in
// place.
func TestOverrideConflictWithCandidate(t *testing.T) {
	e := &env{a: &receiver{name: "A", addr: "127.0.0.1:1"}, b: &receiver{name: "B", addr: "127.0.0.1:2"}}
	before := e.protectedPolicy()
	before.Operations[0].Rate = rate(4, 60)
	overrides := []store.Override{
		{ID: 3, Type: store.OverrideException, Control: store.ControlPermit, Role: "READER", Operation: "write-a", Target: "permit:READER:write-a"},
		{ID: 4, Type: store.OverrideContainment, Control: store.ControlOperation, Operation: "read-b", Target: "operation:read-b"},
		{ID: 5, Type: store.OverrideException, Control: store.ControlRate, Operation: "read-a", Count: 9, Target: "rate:read-a"},
	}
	change := func(edit func(p *policy.Policy)) policy.Policy {
		p := before
		p.Operations = slices.Clone(before.Operations)
		p.Permits = slices.Clone(before.Permits)
		p.Roles = slices.Clone(before.Roles)
		edit(&p)
		return p
	}
	cases := []struct {
		name string
		edit func(p *policy.Policy)
		want int64 // conflicting override, 0 for none
	}{
		{"same permit granted permanently", func(p *policy.Policy) {
			p.Permits = append(p.Permits, policy.Permit{Role: "READER", Operation: "write-a"})
		}, 3},
		{"role of a permit override removed", func(p *policy.Policy) {
			p.Roles = p.Roles[1:] // READER
			p.Permits = slices.DeleteFunc(p.Permits, func(x policy.Permit) bool { return x.Role == "READER" })
		}, 3},
		{"operation of a permit override changed", func(p *policy.Policy) { p.Operations[1].Body = jsonBody(512, anyObject) }, 3},
		{"operation of a permit override removed", func(p *policy.Policy) {
			p.Operations = slices.Delete(p.Operations, 1, 2)
			p.Permits = slices.DeleteFunc(p.Permits, func(x policy.Permit) bool { return x.Operation == "write-a" })
		}, 3},
		{"rate count changed", func(p *policy.Policy) { p.Operations[0].Rate = rate(6, 60) }, 5},
		{"rate rule removed", func(p *policy.Policy) { p.Operations[0].Rate = nil }, 5},
		{"rate window changed", func(p *policy.Policy) { p.Operations[0].Rate = rate(4, 120) }, 5},
		{"contained operation re-routed", func(p *policy.Policy) { p.Operations[3].Target = "/c/*" }, 4},
		{"contained operation denied permanently", func(p *policy.Policy) { p.Operations[3].Access = "deny" }, 4},
		{"more specific operation added inside a contained prefix", func(p *policy.Policy) {
			p.Operations = append(p.Operations, policy.Operation{ID: "special-b", API: "b", Method: "GET", Target: "/b/special", Access: "allow", Auth: "none"})
		}, 4},
		{"more specific prefix added inside an overridden prefix, other method", func(p *policy.Policy) {
			p.Operations = append(p.Operations, policy.Operation{ID: "deep-a", API: "a", Method: "DELETE", Target: "/a/deep/*", Access: "allow", Auth: "none"})
		}, 5},
		{"unrelated permit added", func(p *policy.Policy) {
			p.Permits = append(p.Permits, policy.Permit{Role: "WRITER", Operation: "read-b"})
		}, 0},
		{"unrelated permit removed", func(p *policy.Policy) {
			p.Permits = slices.DeleteFunc(p.Permits, func(x policy.Permit) bool { return x == policy.Permit{Role: "WRITER", Operation: "read-a"} })
		}, 0},
		{"unrelated operation re-routed outside every prefix", func(p *policy.Policy) { p.Operations[2].Target = "/dd" }, 0},
		{"rate rule added to an overridden operation, an independent control", func(p *policy.Policy) { p.Operations[1].Rate = rate(9, 60) }, 0},
		{"less specific operation added above an overridden prefix", func(p *policy.Policy) {
			p.Operations = append(p.Operations, policy.Operation{ID: "all", API: "d", Method: "GET", Target: "/*", Access: "allow", Auth: "none"})
		}, 0},
		{"operation added beside a prefix, outside it", func(p *policy.Policy) {
			p.Operations = append(p.Operations, policy.Operation{ID: "bee", API: "b", Method: "GET", Target: "/bee", Access: "allow", Auth: "none"})
		}, 0},
		{"unrelated role added", func(p *policy.Policy) { p.Roles = append(p.Roles, policy.Role{ID: "OTHER"}) }, 0},
		{"nothing changed", func(p *policy.Policy) {}, 0},
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

	first := e.mustOverride(harness, rateOverride(store.OverrideException, "read-a", 8, 600), `"r1"`, "conflict-first-001")
	refused := []struct {
		name    string
		request OverrideRequest
		reason  string
	}{
		{"same target", rateOverride(store.OverrideException, "read-a", 8, 600), "OVERRIDE_CONFLICT"},
		{"same target, other action", rateOverride(store.OverrideContainment, "read-a", 1, 60), "OVERRIDE_CONFLICT"},
		{"same target, other count and expiry", rateOverride(store.OverrideException, "read-a", 100, 30), "OVERRIDE_CONFLICT"},
		{"case variant", rateOverride(store.OverrideException, "Read-a", 8, 600), "UNKNOWN_REFERENCE"},
		{"path instead of identifier", rateOverride(store.OverrideException, "/a/*", 8, 600), "INVALID_VALUE"},
		{"trailing space", rateOverride(store.OverrideException, "read-a ", 8, 600), "INVALID_VALUE"},
	}
	for i, r := range refused {
		code, outcome, body := e.createOverride(harness, r.request, `"r1"`, fmt.Sprintf("conflict-refused-%02d", i))
		if outcome.Reason != r.reason || (r.reason == "OVERRIDE_CONFLICT" && (code != http.StatusConflict || outcome.Override != first.Override)) {
			t.Errorf("%s: %d %s", r.name, code, body)
		}
	}

	// Independent targets on the same operations coexist.
	independent := []OverrideRequest{
		permitOverride(store.OverrideContainment, "READER", "read-a", 600),
		permitOverride(store.OverrideContainment, "WRITER", "read-a", 600),
		operationContainment("read-a", 600),
		permitOverride(store.OverrideException, "READER", "write-a", 600),
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
		if o.Status == store.OverrideActive {
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
