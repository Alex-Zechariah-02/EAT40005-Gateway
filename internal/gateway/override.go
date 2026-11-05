// Temporary policy override requests, checks and conflicts.

package gateway

import (
	"cmp"
	"slices"
	"strings"
)

// OverrideRequest is an administrator's proposed override. Role and Count are
// pointers so that a member the control does not take is refused rather than
// ignored.
type OverrideRequest struct {
	Type             string  `json:"type"`
	Control          string  `json:"control"`
	Operation        string  `json:"operation"`
	Role             *string `json:"role"`
	Count            *int64  `json:"count"`
	ExpiresInSeconds int64   `json:"expires_in_seconds"`
}

// overrideTarget is the control and target an override governs, in the form
// policyChanges uses, so overrides and candidates are compared by the same
// identity. Operations and roles are named by their policy identifiers, never
// by a path, so no two spellings of one target exist.
func overrideTarget(control, operation, role string) string {
	if control == controlPermit {
		return controlPermit + ":" + role + ":" + operation
	}
	return control + ":" + operation
}

// check validates the request on its own: a supported control and action, the
// members that control takes and no others, well-formed identifiers and a
// lifetime within bounds. Whether the override fits the current policy is
// checked by applyOverrides.
func (r OverrideRequest) check() (override, error) {
	if r.Type != overrideException && r.Type != overrideContainment {
		return override{}, invalid("INVALID_VALUE", "type")
	}
	switch r.Control {
	case controlPermit, controlRate:
	case controlOperation:
		// Allowing an operation the base denies would be a temporary bypass.
		if r.Type != overrideContainment {
			return override{}, invalid("UNSUPPORTED_ACTION", "type")
		}
	default:
		return override{}, invalid("UNSUPPORTED_CONTROL", "control")
	}
	if err := checkID(r.Operation, "operation"); err != nil {
		return override{}, err
	}

	o := override{Type: r.Type, Control: r.Control, Operation: r.Operation}
	if (r.Role != nil) != (r.Control == controlPermit) {
		return override{}, invalid("INVALID_VALUE", "role")
	}
	if r.Role != nil {
		if err := checkID(*r.Role, "role"); err != nil {
			return override{}, err
		}
		o.Role = *r.Role
	}
	if (r.Count != nil) != (r.Control == controlRate) {
		return override{}, invalid("INVALID_VALUE", "count")
	}
	if r.Count != nil {
		if *r.Count < 1 || *r.Count > maxRateCount {
			return override{}, invalid("INVALID_VALUE", "count")
		}
		o.Count = *r.Count
	}
	if r.ExpiresInSeconds < minOverrideSeconds || r.ExpiresInSeconds > maxOverrideSeconds {
		return override{}, invalid("INVALID_EXPIRY", "expires_in_seconds")
	}
	o.Target = overrideTarget(o.Control, o.Operation, o.Role)
	return o, nil
}

// overrideConflict returns the active override that replacing the policy
// before with after would change or invalidate, or nil. That is the case when
// the change touches one of the override's dependencies, or when it adds,
// changes or removes another operation whose target lies inside the
// override's operation's path prefix: such an operation takes requests from
// that prefix, or gives them back, so the override would no longer govern the
// requests it was created for.
func overrideConflict(before, after Policy, overrides []override) *override {
	changed := map[string]bool{}
	var movedTargets []string
	for _, c := range policyChanges(before, after) {
		changed[c.Control+":"+c.Target] = true
		if c.Control != controlOperation {
			continue
		}
		// An operation that keeps its target keeps the paths it takes.
		old, existed := findOperation(before, c.Target)
		current, exists := findOperation(after, c.Target)
		if existed && exists && old.Target == current.Target {
			continue
		}
		if existed {
			movedTargets = append(movedTargets, old.Target)
		}
		if exists {
			movedTargets = append(movedTargets, current.Target)
		}
	}

	ordered := slices.SortedFunc(slices.Values(overrides), func(a, b override) int { return cmp.Compare(a.ID, b.ID) })
	for _, o := range ordered {
		for _, target := range o.dependencies() {
			if changed[target] {
				return &o
			}
		}
		governed, found := findOperation(before, o.Operation)
		if !found {
			return &o
		}
		for _, target := range movedTargets {
			if narrows(governed.Target, target) {
				return &o
			}
		}
	}
	return nil
}

func findOperation(p Policy, id string) (Operation, bool) {
	index := slices.IndexFunc(p.Operations, func(op Operation) bool { return op.ID == id })
	if index < 0 {
		return Operation{}, false
	}
	return p.Operations[index], true
}

// narrows reports whether an operation at target other is matched in
// preference to one at target governed for some of governed's paths. Only a
// prefix target can lose paths, and only to a more specific target beneath
// it, because matching always takes the most specific target.
func narrows(governed, other string) bool {
	prefix, isPrefix := strings.CutSuffix(governed, "*")
	if !isPrefix || other == governed {
		return false
	}
	path := strings.TrimSuffix(other, "*")
	return strings.HasPrefix(path, prefix) && len(path) > len(prefix)
}
