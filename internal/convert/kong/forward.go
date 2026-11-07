// The forwarded path: Kong's strip_path and service path as the API's forward rule.

package kong

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

// routeForward returns the forward rule Kong applies to a route under its
// default path handling (v0): the service path is added, and with strip_path
// (true by default) the matched plain path is removed. It returns a
// refusal when the rewrite cannot be expressed as fixed prefixes.
func routeForward(svc *service, route map[string]any) (policy.Forward, string) {
	var f policy.Forward
	if path := svc.path; path != "" && path != "/" {
		if strings.HasSuffix(path, "/") {
			return f, CodeTransform + ": the service path " + path + " ends with a slash; how Kong joins it to the request path cannot be expressed."
		}
		f.AddPrefix = path
	}
	if route["strip_path"] != false {
		for i, path := range texts(route["paths"]) {
			if strings.HasPrefix(path, "~") {
				return f, CodeTransform + ": Kong strips the part a regular expression matched, which cannot be expressed."
			}
			prefix := strings.TrimSuffix(path, "/")
			if i > 0 && prefix != f.StripPrefix {
				return f, CodeTransform + ": Kong strips a different prefix for each path of the route; one API has one forward rule."
			}
			f.StripPrefix = prefix
		}
	}
	if handling := text(route["path_handling"]); f != (policy.Forward{}) && handling != "" && handling != "v0" {
		return f, CodeTransform + ": path_handling " + handling + " joins paths differently; only v0, Kong's default, can be expressed."
	}
	return f, ""
}

// forwardNote describes a forward rule read from Kong.
func forwardNote(f policy.Forward) string {
	switch {
	case f.StripPrefix != "" && f.AddPrefix != "":
		return "Kong removes " + f.StripPrefix + " and adds the service path " + f.AddPrefix + "; the gateway forwards the same path."
	case f.StripPrefix != "":
		return "Kong removes " + f.StripPrefix + " (strip_path); the gateway forwards the same path."
	}
	return "Kong adds the service path " + f.AddPrefix + "; the gateway forwards the same path."
}

// forwardIn reads an exchange document's forward rule; the zero rule when
// there is none.
func forwardIn(document map[string]any) (policy.Forward, error) {
	var f policy.Forward
	value, present := document[policy.ForwardExtension]
	if !present {
		return f, nil
	}
	data, err := json.Marshal(value)
	if err == nil {
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		err = decoder.Decode(&f)
	}
	if err != nil {
		return f, fmt.Errorf("%s must be {strip_prefix, add_prefix}: %w", policy.ForwardExtension, err)
	}
	return f, nil
}
