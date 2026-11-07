// Tests of the identity, role, failure-mode and schema-dialect primitives.

package semantics

import (
	"testing"

	"github.com/Alex-Zechariah-02/apiward/internal/exchange"
)

func TestIdentity(t *testing.T) {
	strict := Identity{Scheme: IssuerKey, ChecksAudience: true, ChecksExpiry: true}
	lax := Identity{Scheme: IssuerKey}
	for token, want := range map[Token]bool{NoToken: false, Malformed: false, Expired: false, WrongIssuer: false, WrongAudience: false, ValidToken: true} {
		if strict.Authenticates(token) != want {
			t.Errorf("strict, %s: want %v", token, want)
		}
	}
	if !lax.Authenticates(Expired) || !lax.Authenticates(WrongAudience) || lax.Authenticates(WrongIssuer) {
		t.Error("an issuer key without audience and expiry checks decides wrongly")
	}
	cases := []struct {
		identity  Identity
		class     exchange.Class
		direction exchange.Direction
	}{
		{strict, exchange.Equivalent, ""},
		{lax, exchange.Approximated, exchange.Stricter},
		{Identity{Scheme: KeySet, ChecksAudience: true, ChecksExpiry: true}, exchange.Approximated, exchange.Stricter},
		{Identity{Scheme: CallerKey}, exchange.NotExpressible, ""},
		{Identity{Scheme: SharedSecret}, exchange.NotExpressible, ""},
	}
	for _, c := range cases {
		if d := c.identity.Convert(); d.Class != c.class || d.Direction != c.direction || d.Note == "" && c.class != exchange.Equivalent {
			t.Errorf("%+v: %+v", c.identity, d)
		}
	}
}

func TestRoles(t *testing.T) {
	allow := Roles{Source: TokenClaim, Claim: "roles", Rule: AllowList, Values: []string{"admin", "staff"}}
	deny := Roles{Source: TokenClaim, Claim: "roles", Rule: DenyList, Values: []string{"banned"}}
	if !allow.Allows([]string{"staff"}) || allow.Allows([]string{"guest"}) || allow.Allows(nil) {
		t.Error("allow list decides wrongly")
	}
	if !deny.Allows([]string{"guest"}) || !deny.Allows(nil) || deny.Allows([]string{"guest", "banned"}) {
		t.Error("deny list decides wrongly")
	}
	cases := []struct {
		roles     Roles
		class     exchange.Class
		direction exchange.Direction
	}{
		{allow, exchange.Equivalent, ""},
		{Roles{Source: TokenClaim, Claim: "realm.groups", Rule: AllowList}, exchange.Approximated, exchange.Stricter},
		{Roles{Source: GatewayGroups, Rule: AllowList}, exchange.Approximated, exchange.Stricter},
		{deny, exchange.NotExpressible, ""},
	}
	for _, c := range cases {
		if d := c.roles.Convert(); d.Class != c.class || d.Direction != c.direction {
			t.Errorf("%+v: %+v", c.roles, d)
		}
	}
}

func TestFailure(t *testing.T) {
	if FailClosed.Admits() || !FailOpen.Admits() {
		t.Error("failure modes decide wrongly")
	}
	if FailClosed.Convert().Class != exchange.Exact {
		t.Error("fail closed is the gateway's own mode")
	}
	if d := FailOpen.Convert(); d.Class != exchange.Approximated || d.Direction != exchange.Stricter {
		t.Errorf("fail open: %+v", d)
	}
}

func TestDialect(t *testing.T) {
	cases := []struct {
		dialect Dialect
		keyword string
		value   any
		same    bool
	}{
		{Draft2020, "items", map[string]any{}, true},
		{Draft7, "items", []any{map[string]any{}}, false},
		{Draft4, "exclusiveMinimum", true, false},
		{Draft7, "exclusiveMinimum", 3, true},
		{OpenAPI30, "nullable", true, false},
		{OpenAPI30, "maxLength", 4, true},
	}
	for _, c := range cases {
		if c.dialect.Same(c.keyword, c.value) != c.same {
			t.Errorf("%s %s %v: want %v", c.dialect, c.keyword, c.value, c.same)
		}
	}
	if Draft2020.Convert().Class != exchange.Exact || Draft4.Convert().Class != exchange.Equivalent {
		t.Error("dialect distances are wrong")
	}
}
