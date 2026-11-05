// Applying overrides to generations.

package gateway

import (
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

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
	wantPermits := []policy.Permit{{Role: "WRITER", Operation: "write-a"}, {Role: "READER", Operation: "read-b"}, {Role: "READER", Operation: "write-a"}}
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
