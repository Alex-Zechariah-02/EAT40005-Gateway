// Plugin rows: the effective plugins of a route become sign-in, roles, rate and size, or refuse the route.

package kong

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/Alex-Zechariah-02/apiward/internal/exchange"
	"github.com/Alex-Zechariah-02/apiward/internal/semantics"
)

// controls are what a route's plugins require of each of its operations.
type controls struct {
	auth     bool           // sign-in required
	deny     bool           // every caller refused
	roles    []string       // permitted roles; nil when no acl applies
	rate     map[string]any // x-apiward-rate, or nil
	maxBytes int64
	items    []pending
	carried  map[string]any // effective plugin name → its carried config, for x-kong-plugin-*
}

// pending is an item every operation of the route gets.
type pending struct {
	suffix, facet, source string
	bodyOnly              bool
	d                     semantics.Distance
}

// refusal is why a plugin refuses its route.
type refusal struct{ code, message, source string }

// effective returns the plugin that applies per name: route over service
// over global, as Kong resolves scopes; disabled plugins do not apply.
func effective(global, scoped []*plugin) map[string]*plugin {
	out := map[string]*plugin{}
	for _, p := range slices.Concat(global, scoped) {
		if p.object["enabled"] != false {
			out[p.name] = p
		}
	}
	return out
}

// controls maps the effective plugins; an unmappable security plugin
// refuses the whole route, so it is never imported weaker.
func (r *reader) controls(plugins map[string]*plugin) (*controls, *refusal) {
	c := &controls{maxBytes: defaultBodyBytes, carried: map[string]any{}}
	for _, name := range sortedKeys(plugins) {
		p := plugins[name]
		c.carried[name] = map[string]any{"name": name, "config": stripped(p.config, "", nil)}
		if slices.Contains(unsupportedAuth, name) {
			return nil, &refusal{CodeAuth, "the " + name + " plugin signs callers in by a scheme the gateway does not use.", p.pointer}
		}
		if allowed, isMapped := mapped[name]; isMapped {
			if member := unknownMember(p.object, pluginMembers); member != "" {
				return nil, &refusal{CodeUnknown, "the " + name + " plugin member " + member + " is not known.", p.pointer}
			}
			if member := unknownMember(p.config, allowed); member != "" {
				return nil, &refusal{CodeUnknown, "the " + name + " option " + member + " is not known.", p.pointer}
			}
			if protocols := texts(p.object["protocols"]); p.object["protocols"] != nil && !(slices.Contains(protocols, "http") && slices.Contains(protocols, "https")) {
				return nil, &refusal{CodeMatch, "the " + name + " plugin does not run for every protocol.", p.pointer}
			}
		} else if !r.used[p.pointer] {
			r.carriedItem(p)
		}
	}
	steps := []func(*controls, map[string]*plugin) *refusal{r.jwt, acl, rate, size}
	for _, step := range steps {
		if refused := step(c, plugins); refused != nil {
			return nil, refused
		}
	}
	return c, nil
}

// carriedItem gives a plugin the gateway does not enforce its one item.
func (r *reader) carriedItem(p *plugin) {
	r.add("plugin:"+p.scope+":"+p.label, "other", p.pointer, "", semantics.Distance{Class: exchange.NotExpressible,
		Note: "Carried for the way back to Kong; the gateway does not enforce it."})
}

func (c *controls) item(suffix, facet string, p *plugin, d semantics.Distance) {
	c.items = append(c.items, pending{suffix: suffix, facet: facet, source: p.pointer, d: d})
}

// jwt: RS256 credentials whose key is the issuer become trusted issuer
// keys; an anonymous consumer that callers can reach makes it public.
func (r *reader) jwt(c *controls, plugins map[string]*plugin) *refusal {
	p := plugins["jwt"]
	if p == nil {
		return nil
	}
	if claim := text(p.config["key_claim_name"]); claim != "" && claim != "iss" {
		return &refusal{CodeAuth, "jwt selects credentials by the claim " + claim + "; the gateway trusts issuers.", p.pointer}
	}
	if len(r.ledger.Trust.Issuers) == 0 {
		return &refusal{CodeAuth, "no RS256 credential whose key is the issuer is in the file.", p.pointer}
	}
	anonymous := text(p.config["anonymous"])
	if anonymous == "" {
		c.auth = true
		c.item("jwt", "auth", p, semantics.Distance{Class: exchange.Approximated, Direction: exchange.Stricter,
			Note: "Tokens of the issuers' RS256 keys only; the gateway also requires an audience and reads tokens from the Authorization header only (query and cookie tokens are refused)."})
		return nil
	}
	guest := r.consumers[anonymous]
	if guest == nil {
		return &refusal{CodeAuth, "the anonymous consumer " + anonymous + " is not in the file.", p.pointer}
	}
	if gate := plugins["acl"]; gate != nil && !admits(texts(gate.config["allow"]), guest.groups) {
		c.auth = true // the acl admits no group of the anonymous consumer
		c.item("jwt", "auth", p, semantics.Distance{Class: exchange.Approximated, Direction: exchange.Stricter,
			Note: "The anonymous consumer is refused by the acl, so the route is signed-in only; tokens of the issuers' RS256 keys, with an audience, from the Authorization header."})
		return nil
	}
	c.item("jwt", "auth", p, semantics.Distance{Class: exchange.Approximated, Direction: exchange.Looser,
		Note: "Anonymous callers are admitted, so the operation is public: signed-in callers are not identified there, per-account rate limits become per-address, and roles do not apply. Accept the item to import it."})
	return nil
}

// admits reports whether an allow list holds any of the groups.
func admits(allow, groups []string) bool {
	return slices.ContainsFunc(groups, func(g string) bool { return slices.Contains(allow, g) })
}

// acl: an allow list becomes the permitted roles; deny lists are refused.
func acl(c *controls, plugins map[string]*plugin) *refusal {
	p := plugins["acl"]
	if p == nil {
		return nil
	}
	for _, member := range []string{"deny", "allow_when", "deny_when"} {
		if present(p.config[member]) {
			return &refusal{CodeDeny, "acl " + member + " cannot be expressed; the gateway permits listed roles only.", p.pointer}
		}
	}
	allow := texts(p.config["allow"])
	switch {
	case len(allow) == 0:
		return &refusal{CodeDeny, "acl without an allow list cannot be expressed.", p.pointer}
	case !c.auth && plugins["jwt"] == nil:
		c.deny = true
		c.item("acl", "access", p, semantics.Distance{Class: exchange.Equivalent,
			Note: "No plugin signs callers in, so Kong's acl refuses every caller; the operation is denied."})
	case !c.auth:
		c.item("acl", "roles", p, semantics.Distance{Class: exchange.Approximated, Direction: exchange.Looser,
			Note: "Anonymous callers are admitted, so the roles do not apply. Accept the item to import it."})
	default:
		c.roles = allow
		d := semantics.Roles{Source: semantics.GatewayGroups, Rule: semantics.AllowList, Values: allow}.Convert()
		c.item("acl", "roles", p, d)
	}
	return nil
}

// rate: the shortest of Kong's calendar windows becomes a sliding window
// with the same count; longer windows are not enforced (looser).
func rate(c *controls, plugins map[string]*plugin) *refusal {
	p := plugins["rate-limiting"]
	if p == nil {
		return nil
	}
	var found []string
	var count, window int64
	for _, period := range periods {
		number, isNumber := p.config[period.name].(json.Number)
		if !isNumber {
			continue
		}
		if period.seconds == 0 {
			return &refusal{CodeRate, "rate-limiting " + period.name + " windows are longer than the gateway's one day.", p.pointer}
		}
		if len(found) == 0 {
			count, _ = number.Int64()
			window = period.seconds
		}
		found = append(found, period.name)
	}
	if len(found) == 0 {
		return &refusal{CodeRate, "rate-limiting names no window.", p.pointer}
	}
	key := map[string]string{"": "consumer", "consumer": "consumer", "ip": "client_address"}[text(p.config["limit_by"])]
	if key == "" {
		return &refusal{CodeRate, "rate-limiting by " + text(p.config["limit_by"]) + " cannot be expressed.", p.pointer}
	}
	if key == "consumer" {
		key = "client_address" // Kong counts by address when no consumer is signed in
		if c.auth {
			key = "subject"
		}
	}
	n, w, d := semantics.Rate{Algorithm: semantics.FixedAligned, Count: count, Window: window}.Convert()
	if d.Class == exchange.NotExpressible {
		return &refusal{CodeRate, d.Note, p.pointer}
	}
	if len(found) > 1 {
		d = semantics.Distance{Class: exchange.Approximated, Direction: exchange.Looser,
			Note: d.Note + " Only the " + found[0] + " window is kept; " + strings.Join(found[1:], ", ") + " are not enforced. Accept the item to import it."}
	}
	if code := text(p.config["error_code"]); code != "" && code != "429" {
		d.Note += " Kong answered " + code + "; the gateway answers 429."
	}
	if key == "subject" {
		d.Note += " Kong counts per consumer; with issuer credentials all callers of one issuer share one count, the gateway counts each signed-in caller."
	}
	c.rate = map[string]any{"algorithm": "sliding-window", "count": n, "window_seconds": w, "key": key}
	c.item("rate-limiting", "rate", p, d)
	if p.config["fault_tolerant"] != false { // Kong's default is true
		c.item("fault_tolerant", "failure", p, semantics.FailOpen.Convert())
	}
	return nil
}

// size: request-size-limiting becomes the body limit of body operations.
func size(c *controls, plugins map[string]*plugin) *refusal {
	p := plugins["request-size-limiting"]
	if p == nil {
		return nil
	}
	amount := int64(128)
	if number, isNumber := p.config["allowed_payload_size"].(json.Number); isNumber {
		amount, _ = number.Int64()
	}
	unit, isUnit := sizeUnits[cmp.Or(text(p.config["size_unit"]), "megabytes")]
	if !isUnit || amount < 1 {
		return &refusal{CodeUnknown, "request-size-limiting has an unknown unit or size.", p.pointer}
	}
	c.maxBytes = amount * unit
	d := semantics.Distance{Class: exchange.Equivalent, Note: fmt.Sprintf("Bodies of at most %d bytes.", c.maxBytes)}
	if c.maxBytes > maxBodyBytes {
		c.maxBytes = maxBodyBytes
		d = semantics.Distance{Class: exchange.Approximated, Direction: exchange.Stricter,
			Note: fmt.Sprintf("The gateway's largest body is %d bytes.", maxBodyBytes)}
	}
	if p.config["require_content_length"] == true {
		d = semantics.Distance{Class: exchange.Approximated, Direction: exchange.Looser,
			Note: d.Note + " Kong refused bodies without Content-Length; the gateway reads chunked bodies within the limit. Accept the item to import it."}
	}
	c.items = append(c.items, pending{suffix: "request-size-limiting", facet: "size", source: p.pointer, bodyOnly: true, d: d})
	return nil
}
