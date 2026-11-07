// Path primitives: how a route matches a path, how a path is rewritten, and their gateway targets.

package semantics

import (
	"regexp"
	"strings"

	"github.com/Alex-Zechariah-02/apiward/internal/exchange"
)

// Match is a kind of path matching.
type Match string

const (
	Exact               Match = "exact"                 // the path itself
	ElementPrefix       Match = "element_prefix"        // the path and paths under it, by whole segments; a trailing / is ignored
	StringPrefix        Match = "string_prefix"         // any path starting with the text
	DeeperOnly          Match = "deeper_only"           // the gateway's /a/*: /a/x and deeper, not /a or /a/
	LastSegmentTemplate Match = "last_segment_template" // /a/{id}: one non-empty last segment
	Regex               Match = "regex"                 // RE2, matched against the whole path
)

// PathRule is one route's path condition.
type PathRule struct {
	Match Match
	Value string
}

// Matches is the decision rule: whether the route takes the path.
func (r PathRule) Matches(path string) bool {
	switch r.Match {
	case Exact:
		return path == r.Value
	case ElementPrefix:
		base := strings.TrimSuffix(r.Value, "/")
		return base == "" || path == base || strings.HasPrefix(path, base+"/")
	case StringPrefix:
		return strings.HasPrefix(path, r.Value)
	case DeeperOnly:
		rest, ok := strings.CutPrefix(path, strings.TrimSuffix(r.Value, "*"))
		return ok && rest != ""
	case LastSegmentTemplate:
		head, _, templated := lastSegment(r.Value)
		rest, ok := strings.CutPrefix(path, head)
		return templated && ok && rest != "" && !strings.Contains(rest, "/")
	case Regex:
		pattern, err := regexp.Compile(`^(?:` + r.Value + `)$`)
		return err == nil && pattern.MatchString(path)
	}
	return false
}

// Convert returns the gateway targets that together take the route's
// paths, and the distance. Prefixes become an exact target plus a deeper
// one, refusing the siblings a string prefix also takes; a template becomes
// a deeper target, which also takes deeper paths.
func (r PathRule) Convert() ([]string, Distance) {
	switch r.Match {
	case Exact, DeeperOnly:
		return []string{r.Value}, exact()
	case ElementPrefix, StringPrefix:
		return prefixTargets(r)
	case LastSegmentTemplate:
		head, _, templated := lastSegment(r.Value)
		if !templated {
			return nil, inexpressible("Only a template in the last path segment can be expressed.")
		}
		return []string{head + "*"}, approximated(exchange.Looser,
			"The gateway also matches deeper paths under "+head+".")
	}
	return nil, inexpressible("Regular-expression paths cannot be expressed.")
}

func prefixTargets(r PathRule) ([]string, Distance) {
	base := strings.TrimSuffix(r.Value, "/")
	switch {
	case base == "":
		return []string{"/", "/*"}, equivalent("Every path matches, as in the source.")
	case r.Match == StringPrefix && strings.HasSuffix(r.Value, "/"):
		return []string{base + "/*"}, approximated(exchange.Stricter, r.Value+" itself is refused.")
	case r.Match == StringPrefix:
		return []string{base, base + "/*"}, approximated(exchange.Stricter,
			"Paths that only start with the text, such as "+base+"x, and "+base+"/ are refused.")
	}
	return []string{base, base + "/*"}, approximated(exchange.Stricter, base+"/ is refused.")
}

// lastSegment splits /a/{id} into /a/ and {id}; templated is false when
// any other segment holds a template or the last one is not one whole template.
func lastSegment(path string) (string, string, bool) {
	cut := strings.LastIndex(path, "/") + 1
	head, last := path[:cut], path[cut:]
	whole := strings.HasPrefix(last, "{") && strings.HasSuffix(last, "}") && strings.Count(last, "{") == 1
	return head, last, whole && !strings.ContainsAny(head, "{}")
}

// Rewrite is a kind of path transform applied before forwarding.
type Rewrite string

const (
	Unchanged   Rewrite = "identity"     // the path is forwarded unchanged
	StripPrefix Rewrite = "strip_prefix" // Prefix is removed
	Replace     Rewrite = "replace"      // Prefix is replaced by With
)

// Transform is the forwarded path's rule.
type Transform struct {
	Kind   Rewrite
	Prefix string
	With   string
}

// Apply is the decision rule: the path the upstream receives.
func (t Transform) Apply(path string) string {
	rest, ok := strings.CutPrefix(path, t.Prefix)
	if t.Kind == Unchanged || !ok {
		return path
	}
	if t.Kind == Replace {
		rest = strings.TrimSuffix(t.With, "/") + rest
	}
	if !strings.HasPrefix(rest, "/") {
		rest = "/" + rest
	}
	return rest
}

// Convert is the distance: the gateway forwards the path unchanged.
func (t Transform) Convert() Distance {
	if t.Kind == Unchanged {
		return exact()
	}
	return inexpressible("The gateway forwards the path unchanged; a rewrite cannot be expressed.")
}
