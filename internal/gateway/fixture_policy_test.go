// Fixture policy and catalogue agreement.

package gateway

import (
	"slices"
	"strings"
	"testing"
	"time"

	fixture "github.com/Alex-Zechariah-02/EAT40005-Gateway/internal/services/testfixture"
)

// Body schemas of the fixture writes. They state the request shape the
// fixtures accept; business rules such as known items, ownership and the
// quantity range stay with the fixtures.
const (
	ordersCreateSchema = `{"type":"object","additionalProperties":false,"required":["customer","item","quantity"],"properties":{` +
		`"customer":{"type":"string","minLength":1,"maxLength":64},` +
		`"item":{"type":"string","minLength":1,"maxLength":64},` +
		`"quantity":{"type":"integer"}}}`
	inventoryAdjustSchema = `{"type":"object","additionalProperties":false,"required":["delta"],"properties":{` +
		`"delta":{"type":"integer","minimum":-100,"maximum":100}}}`
)

// fixtureRate is the rate rule on every authenticated fixture operation. It
// is wide enough that no catalogue replay reaches it; rate tests use their
// own, tighter rules.
func fixtureRate() *RateRule {
	return &RateRule{Algorithm: "sliding-window", Count: 100, WindowSeconds: 60, Key: "subject"}
}

// fixturePolicy is the gateway policy for the three fixtures. It is written
// here by hand from the fixture contract, not generated from the catalogue,
// so a comparison between the two is meaningful.
func fixturePolicy(fixtures map[string]*fixtureProcess) Policy {
	return Policy{
		SchemaVersion: schemaVersion,
		Profile:       profileCore,
		Upstreams: []Upstream{
			{ID: "orders-svc", Dial: fixtures["orders"].addr},
			{ID: "inventory-svc", Dial: fixtures["inventory"].addr},
			{ID: "reports-svc", Dial: fixtures["reports"].addr},
		},
		APIs: []API{
			{ID: "orders", Upstream: "orders-svc"},
			{ID: "inventory", Upstream: "inventory-svc"},
			{ID: "reports", Upstream: "reports-svc"},
		},
		Operations: []Operation{
			{ID: "orders.list", API: "orders", Method: "GET", Target: "/orders", Access: "allow", Auth: "required", Rate: fixtureRate()},
			{ID: "orders.get", API: "orders", Method: "GET", Target: "/orders/*", Access: "allow", Auth: "required", Rate: fixtureRate()},
			{ID: "orders.create", API: "orders", Method: "POST", Target: "/orders", Access: "allow", Auth: "required",
				Body: jsonBody(1024, ordersCreateSchema), Rate: fixtureRate()},
			{ID: "inventory.health", API: "inventory", Method: "GET", Target: "/inventory/health", Access: "allow", Auth: "none"},
			{ID: "inventory.get", API: "inventory", Method: "GET", Target: "/inventory/items/*", Access: "allow", Auth: "required", Rate: fixtureRate()},
			{ID: "inventory.adjust", API: "inventory", Method: "PATCH", Target: "/inventory/items/*", Access: "allow", Auth: "required",
				Body: jsonBody(1024, inventoryAdjustSchema), Rate: fixtureRate()},
			{ID: "reports.sales", API: "reports", Method: "GET", Target: "/reports/sales", Access: "allow", Auth: "required", Rate: fixtureRate()},
			{ID: "reports.summary", API: "reports", Method: "GET", Target: "/reports/summary", Access: "allow", Auth: "required", Rate: fixtureRate()},
		},
		Roles: []Role{{ID: "CUSTOMER"}, {ID: "STAFF"}, {ID: "MANAGER"}},
		Permits: []Permit{
			{Role: "STAFF", Operation: "orders.list"},
			{Role: "MANAGER", Operation: "orders.list"},
			{Role: "CUSTOMER", Operation: "orders.get"},
			{Role: "STAFF", Operation: "orders.get"},
			{Role: "CUSTOMER", Operation: "orders.create"},
			{Role: "STAFF", Operation: "inventory.get"},
			{Role: "MANAGER", Operation: "inventory.get"},
			{Role: "STAFF", Operation: "inventory.adjust"},
			{Role: "MANAGER", Operation: "reports.sales"},
			{Role: "STAFF", Operation: "reports.summary"},
			{Role: "MANAGER", Operation: "reports.summary"},
		},
	}
}

// bearer is the Authorization header pair carrying a valid token for subject.
func bearer(t *testing.T, subject string) []string {
	t.Helper()
	return []string{"Authorization", "Bearer " + issuer.tokenFor(t, subject)}
}

// catalogueRoles gives each catalogue subject's roles in the form the gateway
// forwards them: sorted and comma-separated.
func catalogueRoles(c fixture.Catalogue) map[string]string {
	roles := map[string]string{}
	for _, s := range c.Subjects {
		roles[s.ID] = strings.Join(slices.Sorted(slices.Values(s.Roles)), ",")
	}
	return roles
}

func loadCatalogue(t *testing.T) fixture.Catalogue {
	t.Helper()
	c, err := fixture.LoadCatalogue()
	if err != nil {
		t.Fatalf("catalogue: %v", err)
	}
	return c
}

// The hand-written gateway policy agrees with the independent catalogue on
// every operation, permit and subject, and the catalogue's role truth
// distinguishes profiles.
func TestPolicyAgreesWithCatalogue(t *testing.T) {
	c := loadCatalogue(t)
	p := fixturePolicy(map[string]*fixtureProcess{
		"orders": {addr: "127.0.0.1:1"}, "inventory": {addr: "127.0.0.1:2"}, "reports": {addr: "127.0.0.1:3"},
	})
	if _, err := Compile(p, 1, time.Time{}); err != nil {
		t.Fatalf("fixture policy does not compile: %v", err)
	}

	roles := map[string][]string{}
	for _, permit := range p.Permits {
		roles[permit.Operation] = append(roles[permit.Operation], permit.Role)
	}
	operations := map[string]Operation{}
	for _, op := range p.Operations {
		operations[op.ID] = op
	}
	if len(operations) != len(c.Operations) {
		t.Fatalf("policy has %d operations, catalogue %d", len(operations), len(c.Operations))
	}

	differing := 0
	for _, want := range c.Operations {
		got, ok := operations[want.ID]
		apiOK := ok && got.API == want.API && got.Method == want.Method && got.Target == want.Target
		if !apiOK || got.Auth != want.Auth || (got.Body != nil) != want.Body {
			t.Errorf("operation %s: policy %+v, catalogue %+v", want.ID, got, want)
		}
		gotRoles := slices.Sorted(slices.Values(roles[want.ID]))
		wantRoles := slices.Sorted(slices.Values(want.Roles))
		if !slices.Equal(gotRoles, wantRoles) {
			t.Errorf("operation %s: policy permits %v, catalogue truth %v", want.ID, gotRoles, wantRoles)
		}
		if len(want.Roles) > 0 && len(want.Roles) < len(c.Roles) {
			differing++
		}
	}
	if differing == 0 {
		t.Fatal("no operation distinguishes role profiles")
	}
	if !slices.Equal(slices.Sorted(slices.Values(c.Roles)), []string{"CUSTOMER", "MANAGER", "STAFF"}) {
		t.Fatalf("catalogue roles %v", c.Roles)
	}

	for _, want := range c.Subjects {
		gotRoles := testRoles(want.ID)
		wantRoles := slices.Sorted(slices.Values(want.Roles))
		if !slices.Equal(gotRoles, wantRoles) {
			t.Errorf("subject %s: issuer %v, catalogue %+v", want.ID, gotRoles, want)
		}
	}
	t.Logf("catalogue %s: %d operations, %d distinguish role profiles, %d subjects, %d scenarios",
		c.Version, len(c.Operations), differing, len(c.Subjects), len(c.Scenarios))
}
