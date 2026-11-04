// Policy count, permit and identifier checks.

package gateway

import "net/netip"

func checkCounts(p Policy) error {
	limits := []struct {
		field string
		count int
		limit int
	}{
		{"upstreams", len(p.Upstreams), maxUpstreams},
		{"apis", len(p.APIs), maxAPIs},
		{"operations", len(p.Operations), maxOperations},
		{"roles", len(p.Roles), maxRoles},
		{"permits", len(p.Permits), maxPermits},
	}
	for _, l := range limits {
		if l.count > l.limit {
			return invalid("LIMIT_EXCEEDED", "%s", l.field)
		}
	}
	return nil
}

// checkPermits validates the role declarations and the permits that refer to
// them, and records each permit on its operation's rule. Unknown roles and
// operations are rejected, so a reference can never grant anything by
// accident. It returns the declared roles.
func checkPermits(p Policy, rules map[string]*rule) (map[string]bool, error) {
	roles := map[string]bool{}
	for i, r := range p.Roles {
		if err := checkID(r.ID, "roles[%d].id", i); err != nil {
			return nil, err
		}
		if roles[r.ID] {
			return nil, invalid("DUPLICATE_ID", "roles[%d].id", i)
		}
		roles[r.ID] = true
	}

	for i, permit := range p.Permits {
		if !roles[permit.Role] {
			return nil, invalid("UNKNOWN_REFERENCE", "permits[%d].role", i)
		}
		r, ok := rules[permit.Operation]
		if !ok {
			return nil, invalid("UNKNOWN_REFERENCE", "permits[%d].operation", i)
		}
		// A permit on an operation that needs no authentication could never
		// apply, so the two declarations contradict each other.
		if r.public {
			return nil, invalid("INVALID_VALUE", "permits[%d].operation", i)
		}
		if r.roles[permit.Role] {
			return nil, invalid("DUPLICATE_ID", "permits[%d]", i)
		}
		r.roles[permit.Role] = true
	}
	return roles, nil
}

// isSubjectID accepts 1 to 128 printable ASCII characters without spaces. The
// same rule applies to the "sub" claim, so a subject identifier is always
// safe to log and to forward in a header.
func isSubjectID(id string) bool {
	if id == "" || len(id) > maxSubjectLength {
		return false
	}
	for i := 0; i < len(id); i++ {
		if id[i] < 0x21 || id[i] > 0x7e {
			return false
		}
	}
	return true
}

// checkID accepts short identifiers made of letters, digits, '.', '_' and
// '-', starting with a letter or digit, so identifiers are safe to log and to
// compare byte for byte.
func checkID(id, field string, args ...any) error {
	if id == "" || len(id) > maxIDLength {
		return invalid("INVALID_VALUE", field, args...)
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		alphanumeric := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		if alphanumeric {
			continue
		}
		if i == 0 || (c != '.' && c != '_' && c != '-') {
			return invalid("INVALID_VALUE", field, args...)
		}
	}
	return nil
}

// isLoopbackDial accepts only literal loopback addresses, so a destination can
// neither be resolved through DNS nor expanded from request placeholders.
func isLoopbackDial(dial string) bool {
	addr, err := netip.ParseAddrPort(dial)
	return err == nil && addr.Port() != 0 && addr.Addr().IsLoopback()
}
