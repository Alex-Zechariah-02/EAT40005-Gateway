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
// method on that target only. A less specific target is never consulted for a
// missing method.
func (g *Generation) Match(path, method string) (*target, *Rule) {
	t := g.exact[path]
	if t == nil {
		for _, candidate := range g.prefixes {
			rest, ok := strings.CutPrefix(path, candidate.prefix)
			if ok && rest != "" {
				t = candidate
				break
			}
		}
	}
	if t == nil {
		return nil, nil
	}
	return t, t.methods[method]
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
