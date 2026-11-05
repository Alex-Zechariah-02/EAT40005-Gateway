// Control changes between two generations.

package compile

import (
	"encoding/json"
	"sort"

	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

// ControlChange names one control and the target it applies to. It is the
// identity by which a candidate's effect on enforcement is reported, and by
// which a temporary override will be compared with a candidate.
type ControlChange struct {
	Control string `json:"control"`
	Target  string `json:"target"`
}

// PolicyChanges lists, in a stable order, every control target whose
// definition differs between two complete policies: an upstream, API,
// operation (route, method, access, authentication, body and header rules),
// its rate rule, a role or a permit (role and operation).
func PolicyChanges(before, after policy.Policy) []ControlChange {
	// encode gives a definition a comparable form; policy types always encode.
	encode := func(value any) string {
		data, _ := json.Marshal(value)
		return string(data)
	}
	// collect maps each control to its targets and their definitions.
	collect := func(p policy.Policy) map[string]map[string]string {
		d := map[string]map[string]string{"upstream": {}, "api": {}, "operation": {}, "rate": {}, "role": {}, "permit": {}}
		for _, u := range p.Upstreams {
			d["upstream"][u.ID] = u.Dial
		}
		for _, a := range p.APIs {
			d["api"][a.ID] = a.Upstream
		}
		for _, op := range p.Operations {
			if op.Rate != nil {
				d["rate"][op.ID] = encode(op.Rate)
			}
			op.Rate = nil
			d["operation"][op.ID] = encode(op)
		}
		for _, r := range p.Roles {
			d["role"][r.ID] = ""
		}
		for _, permit := range p.Permits {
			d["permit"][permit.Role+":"+permit.Operation] = ""
		}
		return d
	}
	old, current := collect(before), collect(after)

	changes := []ControlChange{}
	for control, targets := range current {
		for target, definition := range targets {
			if previous, found := old[control][target]; !found || previous != definition {
				changes = append(changes, ControlChange{control, target})
			}
		}
		for target := range old[control] {
			if _, found := targets[target]; !found {
				changes = append(changes, ControlChange{control, target})
			}
		}
	}
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].Control != changes[j].Control {
			return changes[i].Control < changes[j].Control
		}
		return changes[i].Target < changes[j].Target
	})
	return changes
}
