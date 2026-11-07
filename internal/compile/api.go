// Per-API settings compiled once and shared by the API's rules: the upstream,
// the forward rule, browser access, response headers, the client address
// allow list and the rate rule that limits CORS preflights.

package compile

import (
	"net/netip"

	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

// apiRule is one API's compiled settings.
type apiRule struct {
	upstream  string
	forward   *rewrite      // nil: the inbound path is forwarded unchanged
	cors      *CORS         // nil: no browser access
	response  *ResponseRule // nil: responses unchanged
	addresses []netip.Prefix
	limited   bool      // addresses applies; false admits every client address
	preflight *RateRule // the first public operation's rate rule, if any
}

// compileAPIs checks the policy's APIs against the upstream inventory and
// compiles each API's own settings.
func compileAPIs(p policy.Policy, dials map[string]string) (map[string]*apiRule, error) {
	apis := map[string]*apiRule{}
	for i, a := range p.APIs {
		if err := policy.CheckID(a.ID, "apis[%d].id", i); err != nil {
			return nil, err
		}
		if _, exists := apis[a.ID]; exists {
			return nil, policy.Invalid("DUPLICATE_ID", "apis[%d].id", i)
		}
		if _, ok := dials[a.Upstream]; !ok {
			return nil, policy.Invalid("UNKNOWN_REFERENCE", "apis[%d].upstream", i)
		}
		api := &apiRule{upstream: a.Upstream, limited: a.AllowAddresses != nil}
		var err error
		if api.forward, err = compileForward(a.Forward, i); err != nil {
			return nil, err
		}
		if api.cors, err = compileCORS(a.CORS, i); err != nil {
			return nil, err
		}
		if api.response, err = compileResponse(a.ResponseHeaders, i); err != nil {
			return nil, err
		}
		if api.addresses, err = compileAddresses(a.AllowAddresses, i); err != nil {
			return nil, err
		}
		apis[a.ID] = api
	}
	return apis, nil
}

// compileAddresses checks an allow list: 1 to 256 distinct ranges in
// canonical CIDR form; IPv4 ranges are written as IPv4, never IPv4-mapped,
// because client addresses are compared unmapped.
func compileAddresses(ranges []string, i int) ([]netip.Prefix, error) {
	if ranges == nil {
		return nil, nil
	}
	if len(ranges) < 1 || len(ranges) > policy.MaxAllowAddresses {
		return nil, policy.Invalid("INVALID_VALUE", "apis[%d].allow_addresses", i)
	}
	out := make([]netip.Prefix, 0, len(ranges))
	seen := map[netip.Prefix]bool{}
	for j, text := range ranges {
		prefix, ok := policy.ParseRange(text)
		if !ok || prefix.String() != text || seen[prefix] {
			return nil, policy.Invalid("INVALID_VALUE", "apis[%d].allow_addresses[%d]", i, j)
		}
		seen[prefix] = true
		out = append(out, prefix)
	}
	return out, nil
}

// checkAPIs applies the rules that depend on an API's operations: every CORS
// method is a method of one of them, and origins ["*"] is allowed only
// without credentials on an API whose operations are all public. It also
// picks each API's preflight rate rule: the first public operation's.
func checkAPIs(p policy.Policy, apis map[string]*apiRule, rules map[string]*Rule) error {
	for i, a := range p.APIs {
		api := apis[a.ID]
		methods, public := map[string]bool{}, true
		for _, op := range p.Operations {
			if op.API != a.ID {
				continue
			}
			methods[op.Method] = true
			public = public && op.Auth == "none"
			if api.preflight == nil && op.Auth == "none" {
				api.preflight = rules[op.ID].rate
			}
		}
		if api.cors == nil {
			continue
		}
		for j, method := range a.CORS.Methods {
			if !methods[method] {
				return policy.Invalid("INVALID_VALUE", "apis[%d].cors.methods[%d]", i, j)
			}
		}
		if api.cors.any && (api.cors.credentials || !public) {
			return policy.Invalid("CORS_WILDCARD_NOT_ALLOWED", "apis[%d].cors.origins", i)
		}
	}
	return nil
}
