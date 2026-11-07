// Browser access and address rows: Kong's cors and ip-restriction plugins
// as the API's x-apiward-cors and x-apiward-addresses, both ways.

package kong

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/Alex-Zechariah-02/apiward/internal/exchange"
	"github.com/Alex-Zechariah-02/apiward/internal/policy"
	"github.com/Alex-Zechariah-02/apiward/internal/semantics"
)

// corsMethods are Kong's cors methods when none are listed; the gateway
// keeps those its operations have.
var corsMethods = []string{"GET", "HEAD", "PUT", "PATCH", "POST", "DELETE", "OPTIONS", "TRACE", "CONNECT"}

// browserMaxAge is the preflight cache time browsers use when Kong sends
// no Access-Control-Max-Age.
const browserMaxAge = 5

// corsRule is the browser access one route's cors plugin maps to: value is
// nil when it cannot be expressed, and d says why or how close it is.
type corsRule struct {
	plugin *plugin
	value  map[string]any
	d      semantics.Distance
}

// cors maps Kong's cors plugin to the route's browser access. Without it
// browsers on other origins are refused, so whatever cannot be expressed
// is left out (stricter), never widened.
func cors(c *controls, plugins map[string]*plugin) *refusal {
	p := plugins["cors"]
	if p == nil {
		return nil
	}
	c.cors = &corsRule{plugin: p}
	cfg, d := p.config, semantics.Distance{Class: exchange.Equivalent, Note: "Preflights are answered by the gateway."}
	stricter := func(note string) {
		d = semantics.Distance{Class: exchange.Approximated, Direction: exchange.Stricter, Note: d.Note + " " + note}
	}
	origins := texts(cfg["origins"])
	if len(origins) == 0 || slices.Contains(origins, "*") {
		origins = []string{"*"}
	}
	for i, origin := range origins {
		exact, isExact := exactOrigin(origin)
		switch {
		case origin == "*":
		case isExact && exact == origin && strings.Contains(origin, "."):
			stricter("Kong reads each '.' of " + origin + " as any character; the gateway allows that origin only.")
		case isExact:
			origins[i] = exact
		default:
			c.cors.d = semantics.Distance{Class: exchange.NotExpressible,
				Note: "The origin " + origin + " is a pattern; browser access is not written, so browsers on other origins are refused."}
			return nil
		}
	}
	if origins[0] == "*" && (cfg["credentials"] == true || c.auth) {
		c.cors.d = semantics.Distance{Class: exchange.NotExpressible,
			Note: "Any origin is allowed only on public APIs without credentials; browser access is not written, so browsers on other origins are refused."}
		return nil
	}
	value := map[string]any{"origins": toAny(origins), "methods": toAny(corsMethods), "credentials": cfg["credentials"] == true,
		"headers": toAny(texts(cfg["headers"]))}
	if listed := texts(cfg["methods"]); len(listed) > 0 {
		value["methods"] = toAny(listed)
	}
	if len(texts(cfg["headers"])) == 0 {
		stricter("Kong allowed any requested header; the gateway allows none until headers are listed.")
	}
	if expose := texts(cfg["exposed_headers"]); len(expose) > 0 {
		value["expose"] = toAny(expose)
	}
	value["max_age_seconds"] = json.Number(fmt.Sprint(browserMaxAge))
	if age, isNumber := cfg["max_age"].(json.Number); isNumber {
		value["max_age_seconds"] = age
		if seconds, _ := age.Int64(); seconds > policy.MaxCORSMaxAge {
			value["max_age_seconds"] = json.Number(fmt.Sprint(policy.MaxCORSMaxAge))
			stricter("Browsers cache a preflight answer for at most one day.")
		}
	}
	if cfg["preflight_continue"] == true {
		d = semantics.Distance{Class: exchange.Approximated, Direction: exchange.Both, Note: d.Note + " Kong sent preflights on to the upstream; the gateway answers them itself."}
	}
	if cfg["private_network"] == true {
		stricter("Private network access is not allowed.")
	}
	c.cors.value, c.cors.d = value, d
	return nil
}

// exactOrigin reads a Kong origin as one exact origin: a plain origin, or
// one whose dots are escaped as the writer escapes them. Any other
// regular-expression character makes it a pattern.
func exactOrigin(origin string) (string, bool) {
	plain := strings.ReplaceAll(origin, `\.`, ".")
	if regexp.QuoteMeta(plain) != strings.ReplaceAll(plain, ".", `\.`) || origin != plain && origin != regexp.QuoteMeta(plain) {
		return "", false
	}
	u, err := url.Parse(plain)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.String() != plain ||
		u.Path != "" || u.RawQuery != "" || u.User != nil || strings.ToLower(plain) != plain {
		return "", false
	}
	return plain, true
}

// addresses maps ip-restriction's allow list to the API's allow list; a
// deny list is refused, since the gateway's policy permits listed ranges only.
func addresses(c *controls, plugins map[string]*plugin) *refusal {
	p := plugins["ip-restriction"]
	if p == nil {
		return nil
	}
	if present(p.config["deny"]) {
		return &refusal{CodeDeny, "ip-restriction deny cannot be expressed; the gateway allows listed ranges only.", p.pointer}
	}
	allow := texts(p.config["allow"])
	if len(allow) == 0 || len(allow) > policy.MaxAllowAddresses {
		return &refusal{CodeDeny, "ip-restriction needs an allow list of 1 to 256 entries.", p.pointer}
	}
	var ranges []string
	for _, entry := range allow {
		prefix, err := netip.ParsePrefix(entry)
		if addr, isAddr := netip.ParseAddr(entry); isAddr == nil {
			prefix, err = addr.Prefix(addr.BitLen())
		}
		masked, ok := policy.ParseRange(prefix.Masked().String())
		if err != nil || !ok {
			return &refusal{CodeDeny, "ip-restriction entry " + entry + " is not an address or range.", p.pointer}
		}
		ranges = append(ranges, masked.String())
	}
	slices.Sort(ranges)
	c.allow = slices.Compact(ranges)
	d := semantics.Distance{Class: exchange.Equivalent, Note: "Requests from other addresses are refused with 403."}
	if status, isNumber := p.config["status"].(json.Number); isNumber && status.String() != "403" {
		d = semantics.Distance{Class: exchange.Approximated, Direction: exchange.Both, Note: d.Note + " Kong answered " + status.String() + "."}
	}
	c.item("ip-restriction", "access", p, d)
	return nil
}

// apiSettings writes the API's browser access and allow list into the
// exchange document.
func (r *reader) apiSettings(document map[string]any) {
	if r.allow != nil {
		document[policy.AddressesExtension] = toAny(r.allow)
	}
	if r.browser != nil {
		document[policy.CORSExtension] = r.browser
	}
}

// settleCORS fixes the API's browser access and gives each cors plugin of
// an imported route its item. Browser access is written only when every
// imported route has the same, with the methods of the API's operations;
// otherwise none is (browsers on other origins are refused: stricter).
func (r *reader) settleCORS() {
	var value map[string]any
	agreed := len(r.corsRules) > 0
	for _, rule := range r.corsRules {
		agreed = agreed && rule != nil && rule.value != nil && same(rule.value, r.corsRules[0].value)
	}
	if agreed {
		used := map[string]bool{}
		for _, item := range r.paths {
			for method := range object(item) {
				used[strings.ToUpper(method)] = true
			}
		}
		value = map[string]any{}
		for name, member := range r.corsRules[0].value {
			value[name] = member
		}
		value["methods"] = toAny(slices.DeleteFunc(texts(value["methods"]), func(m string) bool { return !used[m] }))
		r.browser = value
	}
	for _, rule := range r.corsRules {
		if rule == nil || r.used[rule.plugin.pointer] {
			continue
		}
		d, target := rule.d, "#/"+policy.CORSExtension
		if value == nil && d.Class != exchange.NotExpressible {
			d = semantics.Distance{Class: exchange.NotExpressible,
				Note: "The API's routes differ in browser access; one API has one, so none is written and browsers on other origins are refused."}
		}
		if d.Class == exchange.NotExpressible {
			target = ""
		}
		r.add("plugin:"+rule.plugin.scope+":"+rule.plugin.label, "other", rule.plugin.pointer, target, d)
	}
}

// browserPlugins writes the API's browser access and allow list as the
// cors and ip-restriction plugins of a generated route.
func (w *writer) browserPlugins(route string) []any {
	var plugins []any
	if allow := texts(w.document[policy.AddressesExtension]); len(allow) > 0 {
		plugins = append(plugins, map[string]any{"name": "ip-restriction", "config": map[string]any{"allow": allow, "status": 403}})
	}
	value := object(w.document[policy.CORSExtension])
	if value == nil {
		return plugins
	}
	var origins []any
	for _, origin := range texts(value["origins"]) {
		if origin != "*" {
			origin = regexp.QuoteMeta(origin) // Kong reads origins as patterns
		}
		origins = append(origins, origin)
	}
	config := map[string]any{"origins": origins, "methods": value["methods"], "credentials": value["credentials"] == true,
		"max_age": value["max_age_seconds"], "preflight_continue": false, "private_network": false}
	if headers := texts(value["headers"]); len(headers) > 0 {
		config["headers"] = value["headers"]
	} else {
		w.report.Changed = append(w.report.Changed, Change{Route: route, Field: "cors",
			Note: "No request headers are allowed, but Kong allows any requested header when none are listed (looser)."})
	}
	if value["expose"] != nil {
		config["exposed_headers"] = value["expose"]
	}
	if config["max_age"] == nil {
		config["max_age"] = policy.DefaultCORSMaxAge
	}
	return append(plugins, map[string]any{"name": "cors", "config": config})
}
