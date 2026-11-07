// The exchange document a decK file becomes: operations with x-apiward-* and x-kong-* members, and carried values without secrets.

package kong

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/Alex-Zechariah-02/apiward/internal/exchange"
	"github.com/Alex-Zechariah-02/apiward/internal/policy"
	"github.com/Alex-Zechariah-02/apiward/internal/semantics"
)

// anyObject is the body schema of body operations: any JSON object.
var anyObject = map[string]any{"type": "object", "additionalProperties": true}

// describe writes one operation: access, sign-in, roles, rate, body and
// the effective plugins as Kong's own x-kong-plugin-* members.
func (c *controls) describe(operationID, method, target string, prefix bool) map[string]any {
	op := map[string]any{"operationId": operationID, "x-apiward-access": "allow", "security": []any{}}
	if c.deny {
		op["x-apiward-access"] = "deny"
	}
	if prefix {
		op["x-apiward-target"] = target
		op["parameters"] = []any{map[string]any{"name": "rest", "in": "path", "required": true, "schema": map[string]any{"type": "string"}}}
	}
	if c.auth {
		op["security"] = []any{map[string]any{"apiward": []any{}}}
		if c.roles != nil {
			op["x-apiward-roles"] = toAny(c.roles)
		}
	}
	if c.rate != nil {
		op["x-apiward-rate"] = c.rate
	}
	if slices.Contains(bodyMethods, method) {
		op["requestBody"] = map[string]any{"required": true, "content": map[string]any{"application/json": map[string]any{"schema": anyObject}}}
		op["x-apiward-max-bytes"] = c.maxBytes
	}
	for name, value := range c.carried {
		op["x-kong-plugin-"+name] = value
	}
	if c.when != nil {
		op[policy.WhenExtension] = c.when
	}
	return op
}

// bodyDistance is how the gateway's body rule differs from Kong, which
// forwards any body.
func (c *controls) bodyDistance(method string) semantics.Distance {
	note := "Kong forwards any body; the gateway refuses a request with a body here."
	if slices.Contains(bodyMethods, method) {
		note = fmt.Sprintf("Kong forwards any body; the gateway accepts only a JSON object of at most %d bytes, and refuses a request without one.", c.maxBytes)
	}
	return semantics.Distance{Class: exchange.Approximated, Direction: exchange.Stricter, Note: note}
}

// document is the exchange document without its ledger.
func (r *reader) document() map[string]any {
	document := map[string]any{"openapi": "3.1.0", "info": map[string]any{"title": "Kong configuration", "version": "1"},
		"paths": r.paths, "x-kong-route-defaults": map[string]any{"strip_path": false}}
	if r.forward != (policy.Forward{}) {
		forward := map[string]any{}
		for name, prefix := range map[string]string{"strip_prefix": r.forward.StripPrefix, "add_prefix": r.forward.AddPrefix} {
			if prefix != "" {
				forward[name] = prefix
			}
		}
		document[policy.ForwardExtension] = forward
	}
	if r.signedIn {
		document["components"] = map[string]any{"securitySchemes": map[string]any{
			"apiward": map[string]any{"type": "http", "scheme": "bearer", "bearerFormat": "JWT"}}}
	}
	return document
}

// carry keeps a setting for the way back to Kong, without secrets.
func (r *reader) carry(scope, pointer string, value any) {
	var removed []string
	data, err := json.Marshal(stripped(value, "", &removed))
	if err != nil {
		return
	}
	r.skip("plugin_secret_fields", len(removed))
	r.ledger.Carried = append(r.ledger.Carried, exchange.Carried{Vendor: Format, Scope: scope, Path: pointer, Value: data})
}

// stripped returns a copy without members whose names mark a credential
// and without strings holding a private key; removed collects their paths.
func stripped(value any, at string, removed *[]string) any {
	switch v := value.(type) {
	case map[string]any:
		out := map[string]any{}
		for name, child := range v {
			if secretName(name) {
				note(removed, at+"/"+name)
				continue
			}
			out[name] = stripped(child, at+"/"+name, removed)
		}
		return out
	case []any:
		out := make([]any, 0, len(v))
		for i, child := range v {
			out = append(out, stripped(child, fmt.Sprintf("%s/%d", at, i), removed))
		}
		return out
	case string:
		if strings.Contains(v, "PRIVATE KEY") {
			note(removed, at)
			return ""
		}
	}
	return value
}

func note(removed *[]string, path string) {
	if removed != nil {
		*removed = append(*removed, path)
	}
}

// secretName reports whether a member is named as a credential: the word
// itself or ending in _<word> (redis_password, client_secret), but not
// jwt_secrets or secret_is_base64.
func secretName(name string) bool {
	lower := strings.ToLower(name)
	return slices.ContainsFunc(secretWords, func(word string) bool { return lower == word || strings.HasSuffix(lower, "_"+word) })
}

// without returns a copy of an object without the named members.
func without(o map[string]any, names ...string) map[string]any {
	out := maps.Clone(o)
	for _, name := range names {
		delete(out, name)
	}
	return out
}

// unknownMember returns the first member, in order, in none of the lists.
func unknownMember(o map[string]any, lists ...[]string) string {
	for _, name := range sortedKeys(o) {
		if !slices.ContainsFunc(lists, func(l []string) bool { return slices.Contains(l, name) }) {
			return name
		}
	}
	return ""
}

// sortedKeys returns a map's member names in order, so output is deterministic.
func sortedKeys[V any](o map[string]V) []string { return slices.Sorted(maps.Keys(o)) }

func list(value any) []any {
	v, _ := value.([]any)
	return v
}

func object(value any) map[string]any {
	v, _ := value.(map[string]any)
	return v
}

func text(value any) string {
	v, _ := value.(string)
	return v
}

// texts returns the strings of a list.
func texts(value any) []string {
	var out []string
	for _, item := range list(value) {
		if s, isText := item.(string); isText {
			out = append(out, s)
		}
	}
	return out
}

func toAny(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

// present reports whether a member holds anything: a non-empty list or object, or another value.
func present(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case []any:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	}
	return true
}

// reference reads an entity reference: a name or id, or {name} / {id}.
func reference(value any) string {
	if s := text(value); s != "" {
		return s
	}
	o := object(value)
	if name := text(o["name"]); name != "" {
		return name
	}
	return text(o["id"])
}

// pluginName is a plugin's label in item IDs: its name, and its instance name when it has one.
func pluginName(p map[string]any) string {
	if instance := text(p["instance_name"]); instance != "" {
		return text(p["name"]) + "." + instance
	}
	return text(p["name"])
}
