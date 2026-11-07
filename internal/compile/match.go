// Canonical paths, route matching and role authorisation.

package compile

import (
	"net/http"
	"strings"

	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

// parseTarget returns the match prefix for "/a/*" targets, or "" for exact
// targets. Targets must already be canonical.
func parseTarget(pattern string) (string, bool) {
	if len(pattern) > policy.MaxTargetLength {
		return "", false
	}
	path, isPrefix := strings.CutSuffix(pattern, "/*")
	if isPrefix {
		path += "/"
	}
	if !IsCanonical(path) || strings.Contains(path, "*") {
		return "", false
	}
	if isPrefix {
		return path, true
	}
	return "", true
}

// CanonicalPath returns the single path interpretation used for matching and
// forwarding. Any encoding whose meaning could differ between the gateway and
// an upstream is rejected rather than normalised.
func CanonicalPath(r *http.Request) (string, bool) {
	escaped := strings.ToLower(r.URL.EscapedPath())
	for _, banned := range []string{"%2f", "%5c", "%2e", "%00"} {
		if strings.Contains(escaped, banned) {
			return "", false
		}
	}
	path := r.URL.Path
	if !IsCanonical(path) {
		return "", false
	}
	return path, true
}

func IsCanonical(path string) bool {
	if !strings.HasPrefix(path, "/") {
		return false
	}
	for i := 0; i < len(path); i++ {
		c := path[i]
		if c < 0x21 || c > 0x7e || c == '\\' || c == '%' {
			return false
		}
	}
	segments := strings.Split(path[1:], "/")
	for i, segment := range segments {
		if segment == "." || segment == ".." {
			return false
		}
		// Only a single trailing empty segment (a trailing slash) is allowed.
		if segment == "" && i != len(segments)-1 {
			return false
		}
	}
	return true
}

// Match selects the most specific target for a canonical path, then the
// method on that target only, then among that method's operations the one
// with the most conditions the request meets, else the unconditioned one. A
// less specific target is never consulted for a missing method. When the
// method has only conditioned operations and none holds, no target matches.
func (g *Generation) Match(path, method string, in Inbound) (*target, *Rule) {
	t := g.lookup(path)
	if t == nil {
		return nil, nil
	}
	if variants := t.variants[method]; len(variants) > 0 {
		host := requestHost(in.Host)
		for _, v := range variants {
			if v.when.holds(host, in.Header) {
				return t, v
			}
		}
		if t.methods[method] == nil {
			return nil, nil
		}
	}
	return t, t.methods[method]
}

// Conditioned reports whether the method on the path's target has
// conditioned operations, so the operation depends on host and headers.
func (g *Generation) Conditioned(path, method string) bool {
	t := g.lookup(path)
	return t != nil && len(t.variants[method]) > 0
}

// Preflight returns the rule a CORS preflight's requested method selects on
// the path, as Match would for the actual request, and whether the path's
// target has an operation of an API with browser access; only then does the
// gateway answer the preflight.
func (g *Generation) Preflight(path, method string, in Inbound) (*Rule, bool) {
	t := g.lookup(path)
	if t == nil || !t.cors {
		return nil, false
	}
	_, rule := g.Match(path, method, in)
	return rule, true
}

// lookup returns the most specific target for a canonical path.
func (g *Generation) lookup(path string) *target {
	if t := g.exact[path]; t != nil {
		return t
	}
	for _, candidate := range g.prefixes {
		rest, ok := strings.CutPrefix(path, candidate.prefix)
		if ok && rest != "" {
			return candidate
		}
	}
	return nil
}

// Authorise retains only declared roles from already verified claims. Unknown
// roles are returned for audit; a subject name never creates a permission.
func (g *Generation) Authorise(asserted []string, r *Rule) (roles, ignored []string, reason string) {
	allowed := false
	for _, role := range asserted {
		if g.roles[role] {
			roles = append(roles, role)
			allowed = allowed || r.roles[role]
		} else {
			ignored = append(ignored, role)
		}
	}
	if !allowed {
		reason = "no_permit"
	}
	return
}
