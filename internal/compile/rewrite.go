// Forwarded path rewrite: the compiler's checks of an API's forward rule and
// the destination an admitted request is forwarded to.

package compile

import (
	"strings"

	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

// rewrite is a compiled forward rule: fixed strings, never patterns.
type rewrite struct{ strip, add string }

// Destination is where an admitted request goes: its upstream's address and
// the path it is forwarded with. Only Rule.Destination makes one, after the
// rewritten path passed the canonical-path check, so a forwarder bound to a
// Destination cannot be handed an unchecked path.
type Destination struct{ dial, path string }

// Dial is the upstream's loopback address.
func (d Destination) Dial() string { return d.dial }

// Path is the canonical path the request is forwarded with.
func (d Destination) Path() string { return d.path }

// compileForward checks an API's forward rule: each prefix is a canonical
// path without a trailing slash, wildcard or encoded character, and at
// least one is set. A nil rule forwards the path unchanged.
func compileForward(f *policy.Forward, i int) (*rewrite, error) {
	if f == nil {
		return nil, nil
	}
	if f.StripPrefix == "" && f.AddPrefix == "" {
		return nil, policy.Invalid("REWRITE_INVALID", "apis[%d].forward", i)
	}
	for _, p := range []struct{ name, value string }{{"strip_prefix", f.StripPrefix}, {"add_prefix", f.AddPrefix}} {
		if p.value == "" {
			continue
		}
		if len(p.value) > policy.MaxTargetLength || !IsCanonical(p.value) || strings.HasSuffix(p.value, "/") || strings.Contains(p.value, "*") {
			return nil, policy.Invalid("REWRITE_INVALID", "apis[%d].forward.%s", i, p.name)
		}
	}
	return &rewrite{strip: f.StripPrefix, add: f.AddPrefix}, nil
}

// under reports whether a target lies under the strip prefix at a segment
// boundary, so every path it matches keeps a canonical remainder.
func (w *rewrite) under(target string) bool {
	return w == nil || w.strip == "" || target == w.strip || strings.HasPrefix(target, w.strip+"/")
}

// Destination returns where a request admitted on the canonical inbound
// path is forwarded. The path is the inbound path without the strip prefix,
// after the add prefix ("/" when nothing is left), and must be canonical
// again; otherwise ok is false and the request must not be forwarded.
func (r *Rule) Destination(path string) (d Destination, ok bool) {
	out := path
	if r.forward != nil {
		rest, cut := strings.CutPrefix(path, r.forward.strip)
		if !cut || (rest != "" && rest[0] != '/') {
			return Destination{}, false
		}
		out = r.forward.add + rest
		if out == "" {
			out = "/"
		}
	}
	if !IsCanonical(out) {
		return Destination{}, false
	}
	return Destination{dial: r.dial, path: out}, true
}
