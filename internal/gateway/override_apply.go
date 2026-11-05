// Applying active overrides to a base generation.

package gateway

import (
	"cmp"
	"slices"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/compile"
	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

// applyOverrides returns a copy of base with every override applied. Each
// override must still change what its type says against this base: an
// exception must grant a permit the base lacks or raise its rate count, a
// containment must remove a permit the base has, lower its rate count or deny
// an operation the base allows. Two overrides of one target, or one that no
// longer fits, reject the whole result. The overrides are applied in
// identifier order and govern distinct targets, so neither the order they are
// given in nor any precedence decides the outcome. base itself is not changed.
func applyOverrides(base policy.Policy, overrides []override) (policy.Policy, error) {
	p := base
	p.Operations = slices.Clone(base.Operations)
	p.Permits = slices.Clone(base.Permits)

	ordered := slices.SortedFunc(slices.Values(overrides), func(a, b override) int { return cmp.Compare(a.ID, b.ID) })
	targets := map[string]bool{}
	for _, o := range ordered {
		if targets[o.Target] {
			return policy.Policy{}, policy.Invalid("OVERRIDE_CONFLICT", "%s", o.Target)
		}
		targets[o.Target] = true
		if err := applyOverride(&p, o); err != nil {
			return policy.Policy{}, err
		}
	}
	return p, nil
}

// applyOverride applies one override to p, whose operation and permit slices
// belong to the caller.
func applyOverride(p *policy.Policy, o override) error {
	index := slices.IndexFunc(p.Operations, func(op policy.Operation) bool { return op.ID == o.Operation })
	if index < 0 {
		return policy.Invalid("UNKNOWN_REFERENCE", "operation")
	}
	op := &p.Operations[index]
	exception := o.Type == overrideException

	switch o.Control {
	case controlPermit:
		if !slices.ContainsFunc(p.Roles, func(r policy.Role) bool { return r.ID == o.Role }) {
			return policy.Invalid("UNKNOWN_REFERENCE", "role")
		}
		// A public operation reads no token, so a permit on it means nothing.
		if op.Auth != "required" {
			return policy.Invalid("INVALID_VALUE", "operation")
		}
		permit := policy.Permit{Role: o.Role, Operation: o.Operation}
		held := slices.Index(p.Permits, permit)
		switch {
		case exception && held >= 0:
			return policy.Invalid("INVALID_VALUE", "role")
		case exception && op.Access != "allow":
			// A permit cannot open an operation the base denies.
			return policy.Invalid("INVALID_VALUE", "operation")
		case exception:
			p.Permits = append(p.Permits, permit)
		case held < 0:
			return policy.Invalid("INVALID_VALUE", "role")
		default:
			p.Permits = slices.Delete(p.Permits, held, held+1)
		}

	case controlRate:
		if op.Rate == nil {
			return policy.Invalid("INVALID_VALUE", "operation")
		}
		// Only the count changes: the algorithm, window and counting key stay
		// the base rule's, so the counters already accrued keep applying.
		raises := o.Count > op.Rate.Count
		lowers := o.Count < op.Rate.Count
		if (exception && !raises) || (!exception && !lowers) {
			return policy.Invalid("INVALID_VALUE", "count")
		}
		rule := *op.Rate
		rule.Count = o.Count
		op.Rate = &rule

	case controlOperation:
		if o.Type != overrideContainment || op.Access != "allow" {
			return policy.Invalid("INVALID_VALUE", "operation")
		}
		op.Access = "deny"

	default:
		return policy.Invalid("UNSUPPORTED_CONTROL", "control")
	}
	return nil
}

// effectiveGeneration compiles the active base with the active overrides into
// one generation. It expires at the earliest of the provisional deadline (if
// any) and the overrides' expiry times, so a generation never serves past the
// first of them. Its identity names the base revision and every override.
func effectiveGeneration(base policy.Policy, revision int64, deadline time.Time, overrides []override) (*compile.Generation, error) {
	if len(overrides) > maxActiveOverrides {
		return nil, policy.Invalid("LIMIT_EXCEEDED", "overrides")
	}
	p, err := applyOverrides(base, overrides)
	if err != nil {
		return nil, err
	}
	notAfter := deadline
	ids := make([]int64, 0, len(overrides))
	for _, o := range overrides {
		if notAfter.IsZero() || o.ExpiresAt.Before(notAfter) {
			notAfter = o.ExpiresAt
		}
		ids = append(ids, o.ID)
	}
	slices.Sort(ids)

	g, err := compile.Compile(p, revision, notAfter)
	if err != nil {
		return nil, err
	}
	if len(ids) > 0 {
		// Not yet published, so the generation can still be labelled.
		g.Overrides = ids
		g.ID = compile.GenerationID(p, revision, notAfter, ids)
	}
	return g, nil
}
