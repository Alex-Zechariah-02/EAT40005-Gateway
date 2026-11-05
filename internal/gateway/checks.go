// Policy count, permit and upstream address checks.

package gateway

import (
	"net/netip"

	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

func checkCounts(p policy.Policy) error {
	limits := []struct {
		field string
		count int
		limit int
	}{
		{"upstreams", len(p.Upstreams), policy.MaxUpstreams},
		{"apis", len(p.APIs), policy.MaxAPIs},
		{"operations", len(p.Operations), policy.MaxOperations},
		{"roles", len(p.Roles), policy.MaxRoles},
		{"permits", len(p.Permits), policy.MaxPermits},
	}
	for _, l := range limits {
		if l.count > l.limit {
			return policy.Invalid("LIMIT_EXCEEDED", "%s", l.field)
		}
	}
	return nil
}

// checkPermits validates the role declarations and the permits that refer to
// them, and records each permit on its operation's rule. Unknown roles and
// operations are rejected, so a reference can never grant anything by
// accident. It returns the declared roles.
func checkPermits(p policy.Policy, rules map[string]*rule) (map[string]bool, error) {
	roles := map[string]bool{}
	for i, r := range p.Roles {
		if err := policy.CheckID(r.ID, "roles[%d].id", i); err != nil {
			return nil, err
		}
		if roles[r.ID] {
			return nil, policy.Invalid("DUPLICATE_ID", "roles[%d].id", i)
		}
		roles[r.ID] = true
	}

	for i, permit := range p.Permits {
		if !roles[permit.Role] {
			return nil, policy.Invalid("UNKNOWN_REFERENCE", "permits[%d].role", i)
		}
		r, ok := rules[permit.Operation]
		if !ok {
			return nil, policy.Invalid("UNKNOWN_REFERENCE", "permits[%d].operation", i)
		}
		// A permit on an operation that needs no authentication could never
		// apply, so the two declarations contradict each other.
		if r.public {
			return nil, policy.Invalid("INVALID_VALUE", "permits[%d].operation", i)
		}
		if r.roles[permit.Role] {
			return nil, policy.Invalid("DUPLICATE_ID", "permits[%d]", i)
		}
		r.roles[permit.Role] = true
	}
	return roles, nil
}

// isLoopbackDial accepts only literal loopback addresses, so a destination can
// neither be resolved through DNS nor expanded from request placeholders.
func isLoopbackDial(dial string) bool {
	addr, err := netip.ParseAddrPort(dial)
	return err == nil && addr.Port() != 0 && addr.Addr().IsLoopback()
}
