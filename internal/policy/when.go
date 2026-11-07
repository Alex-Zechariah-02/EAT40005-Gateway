// Host and header conditions that select among operations sharing a target
// and method.

package policy

// When lists the conditions an operation's requests must meet: the request
// host matches one of Hosts (exact, or "*." followed by a domain, which
// matches any name ending in "."+domain) and every header condition holds.
// Conditions only select an operation; they never grant access.
//
// In an OpenAPI description or exchange file the conditions are the
// operation member named WhenExtension.
type When struct {
	Hosts   []string          `json:"hosts,omitempty"`
	Headers []HeaderCondition `json:"headers,omitempty"`
}

// HeaderCondition holds when the request carries the named header exactly
// once, with exactly this value. Names compare without regard to case.
type HeaderCondition struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// WhenExtension is the OpenAPI operation member that carries a When.
const WhenExtension = "x-apiward-when"

const (
	MaxWhenHosts   = 16
	MaxWhenHeaders = 8
)
