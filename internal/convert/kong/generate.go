// Routes written from operations: anchored regular-expression paths and the plugins each operation's rules need.

package kong

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

// Regular-expression priorities: an exact path before any prefix, and a
// longer prefix before a shorter one, as the gateway matches.
const exactPriority = 100000

// unclaimed writes the operations no carried route made, in path order.
func (w *writer) unclaimed() []any {
	out := []any{}
	paths := object(w.document["paths"])
	for _, path := range sortedKeys(paths) {
		for _, method := range methods {
			lower := strings.ToLower(method)
			t := "#/paths/" + escape(path) + "/" + lower
			if op := object(object(paths[path])[lower]); op != nil && !w.claimed[t] {
				w.claimed[t] = true
				if generated := w.generate(t, op, nil); generated != nil {
					out = append(out, generated)
				}
			}
		}
	}
	return out
}

// generate writes one route for one operation, or nil when Kong cannot
// forward it as the gateway does. original is the Kong route the operation
// came from, whose plugins the gateway does not enforce are kept; nil for an
// operation from another source.
func (w *writer) generate(pointer string, op map[string]any, original map[string]any) map[string]any {
	path, method, _ := strings.Cut(strings.TrimPrefix(pointer, "#/paths/"), "/")
	path = strings.NewReplacer("~1", "/", "~0", "~").Replace(path)
	name := strings.ReplaceAll(text(op["operationId"]), ".", "-")
	if strip := w.forward.StripPrefix; strip != "" {
		// Kong strips the whole match of a regular-expression path, and a
		// plain path also matches longer paths, so neither can be written.
		w.report.NotCarried = append(w.report.NotCarried, Change{Route: name, Field: "forward", Note: "Not written: Kong cannot remove " +
			strip + " from an exactly matched route. Add the route by hand (paths [" + strip + "], strip_path true) if its wider match is acceptable."})
		return nil
	}
	pattern, priority := pathPattern(path, text(op["x-apiward-target"]))
	route := map[string]any{"name": name, "paths": []any{pattern}, "methods": []any{strings.ToUpper(method)},
		"strip_path": false, "regex_priority": priority}
	putWhen(route, object(op[policy.WhenExtension]))
	plugins := slices.Concat(w.rules(name, op), w.browserPlugins(name), w.kept(op, original))
	if len(plugins) > 0 {
		route["plugins"] = plugins
	}
	w.report.Generated = append(w.report.Generated, name)
	from := "the operation"
	if original != nil {
		from = "the operation, because it changed since the import of route " + text(original["name"])
	}
	w.report.Changed = append(w.report.Changed, Change{Route: name, Field: "route", Note: "Written from " + from + "."})
	return route
}

// kept returns the plugins the gateway does not enforce that applied to
// the route alone: the original route's own and those of the root list
// that named it, which are then dropped there; for an operation from
// another source, its x-kong-plugin-* members.
func (w *writer) kept(op, original map[string]any) []any {
	var out []any
	if original == nil {
		for _, key := range sortedKeys(op) {
			kind, isKong := strings.CutPrefix(key, "x-kong-plugin-")
			if _, isMapped := mapped[kind]; isKong && !isMapped {
				out = append(out, op[key])
			}
		}
		return out
	}
	w.dropped[text(original["name"])], w.dropped[text(original["id"])] = true, true
	for _, p := range slices.Concat(list(original["plugins"]), w.rootScoped(original)) {
		if _, isMapped := mapped[text(object(p)["name"])]; !isMapped {
			out = append(out, without(object(p), "route", "id"))
		}
	}
	return out
}

// rootScoped returns the root-level plugins that name a route.
func (w *writer) rootScoped(route map[string]any) []any {
	var out []any
	for _, p := range list(w.root["plugins"]) {
		ref := reference(object(p)["route"])
		if ref != "" && (ref == text(route["name"]) || ref == text(route["id"])) {
			out = append(out, p)
		}
	}
	return out
}

// pathPattern is the anchored Kong path for a target: a plain Kong path
// is a string prefix, which would widen an exact target.
func pathPattern(path, target string) (string, int) {
	if prefix, isPrefix := strings.CutSuffix(target, "*"); isPrefix {
		return "~" + regexp.QuoteMeta(prefix) + ".+", len(prefix)
	}
	var b strings.Builder
	for i, segment := range strings.Split(path, "/") {
		if i > 0 {
			b.WriteString("/")
		}
		if strings.HasPrefix(segment, "{") && strings.HasSuffix(segment, "}") {
			b.WriteString("[^/]+") // an OpenAPI template is one segment
			continue
		}
		b.WriteString(regexp.QuoteMeta(segment))
	}
	return "~" + b.String() + "$", exactPriority
}

// rules writes the plugins for the operation's gateway rules, fail closed,
// and lists what Kong cannot carry.
func (w *writer) rules(route string, op map[string]any) []any {
	var plugins []any
	signedIn := len(list(op["security"])) > 0
	if signedIn {
		config := without(object(object(op["x-kong-plugin-jwt"])["config"]), "anonymous")
		if len(config) == 0 {
			config = map[string]any{"claims_to_verify": []any{"exp"}}
		}
		plugins = append(plugins, map[string]any{"name": "jwt", "config": config})
	}
	roles, hasRoles := op["x-apiward-roles"]
	switch {
	case text(op["x-apiward-access"]) == "deny" || signedIn && hasRoles && len(list(roles)) == 0:
		plugins = append(plugins, map[string]any{"name": "request-termination",
			"config": map[string]any{"status_code": 403, "message": "Forbidden"}})
	case signedIn && hasRoles:
		plugins = append(plugins, map[string]any{"name": "acl", "config": map[string]any{"allow": roles}})
		w.report.ManualSteps = append(w.report.ManualSteps, "Give the consumers of route "+route+" the acl groups that match their roles.")
	}
	if rate := object(op["x-apiward-rate"]); rate != nil {
		plugins = append(plugins, w.rate(route, rate)...)
	}
	if size := number(op["x-apiward-max-bytes"]); size > 0 {
		plugins = append(plugins, map[string]any{"name": "request-size-limiting",
			"config": map[string]any{"allowed_payload_size": size, "size_unit": "bytes", "require_content_length": false}})
	}
	schema := object(object(object(object(op["requestBody"])["content"])["application/json"])["schema"])
	if schema != nil && !same(schema, anyObject) {
		w.report.NotCarried = append(w.report.NotCarried, Change{Route: route, Field: "body",
			Note: "Kong OSS does not validate request bodies; the schema is not written."})
	}
	if len(list(op["parameters"])) > 0 && op["x-apiward-target"] == nil {
		w.report.NotCarried = append(w.report.NotCarried, Change{Route: route, Field: "headers",
			Note: "Kong OSS does not validate headers; the header schema is not written."})
	}
	return plugins
}

// rate writes a sliding window as Kong's calendar window of the same count
// and length, fail closed; other lengths are not carried.
func (w *writer) rate(route string, rate map[string]any) []any {
	count, window := number(rate["count"]), number(rate["window_seconds"])
	limitBy := map[string]string{"subject": "consumer", "client_address": "ip"}[text(rate["key"])]
	for _, period := range periods {
		if period.seconds == window && limitBy != "" {
			w.report.Changed = append(w.report.Changed, Change{Route: route, Field: "rate", Note: fmt.Sprintf(
				"%d per %d s becomes Kong's fixed calendar window: at most %d in any %d s instead of %d, and %d always admitted instead of %d.",
				count, window, 2*count, window, 2*count-1, count, count/2)})
			return []any{map[string]any{"name": "rate-limiting", "config": map[string]any{
				period.name: count, "limit_by": limitBy, "policy": "local", "fault_tolerant": false}}}
		}
	}
	w.report.NotCarried = append(w.report.NotCarried, Change{Route: route, Field: "rate",
		Note: "Kong's windows are 1 s, 1 min, 1 h or 1 day; a " + strconv.FormatInt(window, 10) + " s window is not written."})
	return nil
}

// identity writes the trusted issuers as consumers only with --with-trust;
// consumers from the file are never written.
func (w *writer) identity(out map[string]any, consumers []any) {
	if len(consumers) > 0 {
		w.report.ManualSteps = append(w.report.ManualSteps, fmt.Sprintf(
			"Recreate the %d consumers of the Kong file (names and groups); they are not part of the API configuration.", len(consumers)))
	}
	if w.options.WithBlocked && len(w.ledger.Blocked) > 0 {
		w.report.ManualSteps = append(w.report.ManualSteps, "Kong has no blocked-account list; block those accounts in the identity provider.")
	}
	issuers := w.ledger.Trust.Issuers
	if len(issuers) == 0 {
		return
	}
	if !w.options.WithTrust {
		w.report.ManualSteps = append(w.report.ManualSteps, fmt.Sprintf(
			"Configure the %d trusted issuers as jwt consumers (--with-trust writes them).", len(issuers)))
		return
	}
	var written []any
	for i, issuer := range issuers {
		var secrets []any
		for _, key := range issuer.PublicKeys {
			// Kong requires a secret even for RS256, where it is never used;
			// a digest of the public key is deterministic and reveals nothing.
			digest := sha256.Sum256([]byte(key))
			secrets = append(secrets, map[string]any{"key": issuer.Issuer, "algorithm": "RS256",
				"rsa_public_key": key, "secret": hex.EncodeToString(digest[:])})
		}
		if secrets == nil {
			w.report.ManualSteps = append(w.report.ManualSteps, "Add the public keys of the issuer "+issuer.Issuer+".")
			continue
		}
		written = append(written, map[string]any{"username": "issuer-" + strconv.Itoa(i+1), "jwt_secrets": secrets})
	}
	if written != nil {
		out["consumers"] = written
	}
}

// number reads an integer from a decoded or a Go value; 0 when it is none.
func number(value any) int64 {
	switch v := value.(type) {
	case json.Number:
		n, _ := v.Int64()
		return n
	case int64:
		return v
	case int:
		return int64(v)
	}
	return 0
}
