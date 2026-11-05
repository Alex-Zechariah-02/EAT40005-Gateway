// Read-only access to a compiled generation's rules: a published generation
// is shared by concurrent requests, so callers read it and never change it.

package compile

import (
	"cmp"
	"iter"
	"maps"
	"slices"
)

// Operation is the policy operation the rule enforces.
func (r *Rule) Operation() string { return r.operation }

// API is the policy API the operation belongs to.
func (r *Rule) API() string { return r.api }

// Upstream is the ID of the upstream the operation is bound to.
func (r *Rule) Upstream() string { return r.upstream }

// Dial is the loopback address of that upstream.
func (r *Rule) Dial() string { return r.dial }

// Permitted reports whether the operation's access flag allows requests.
func (r *Rule) Permitted() bool { return r.permitted }

// Public reports an operation declared auth "none": no token is read.
func (r *Rule) Public() bool { return r.public }

// Body is the compiled body rule; nil when the operation accepts no body.
func (r *Rule) Body() *BodyRule { return r.body }

// Headers is the compiled header schema; nil when there is none.
func (r *Rule) Headers() *headerRule { return r.headers }

// Rate is the compiled rate rule; nil when the operation has no rate limit.
func (r *Rule) Rate() *RateRule { return r.rate }

// RateKey says whether the rate limit counts per subject or per peer address.
func (r *Rule) RateKey() string { return r.rateKey }

// Roles lists, sorted, the roles holding a permit for the operation.
func (r *Rule) Roles() []string { return slices.Sorted(maps.Keys(r.roles)) }

// Roles lists, sorted, the roles the generation's policy declares.
func (g *Generation) Roles() []string { return slices.Sorted(maps.Keys(g.roles)) }

// Allow is the value of the Allow header for the target.
func (t *target) Allow() string { return t.allow }

// MaxBytes is the largest body the operation accepts.
func (b *BodyRule) MaxBytes() int64 { return b.maxBytes }

// Validate checks a decoded body against the operation's schema.
func (b *BodyRule) Validate(document any) error { return b.schema.Validate(document) }

// Names yields the header names the schema declares.
func (h *headerRule) Names() iter.Seq[string] { return slices.Values(h.names) }

// Declares reports whether the schema declares the header name.
func (h *headerRule) Declares(name string) bool { return slices.Contains(h.names, name) }

// Validate checks the declared headers against the schema.
func (h *headerRule) Validate(headers any) error { return h.schema.Validate(headers) }

// Count is the number of requests allowed per window.
func (r *RateRule) Count() int64 { return r.count }

// Window is the window length in milliseconds.
func (r *RateRule) Window() int64 { return r.window }

// Rules lists every compiled endpoint rule of a generation once.
func (g *Generation) Rules() []*Rule {
	var list []*Rule
	add := func(t *target) {
		for _, r := range t.methods {
			list = append(list, r)
		}
	}
	for _, t := range g.exact {
		add(t)
	}
	for _, t := range g.prefixes {
		add(t)
	}
	slices.SortFunc(list, func(a, b *Rule) int { return cmp.Compare(a.operation, b.operation) })
	return list
}
