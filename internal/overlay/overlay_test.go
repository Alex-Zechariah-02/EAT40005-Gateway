// Tests of the decisions profile: allowed and refused files, stale and
// multiple matches, order, and decisions that would widen access.

package overlay

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/Alex-Zechariah-02/apiward/internal/exchange"
)

// document is a small exchange tree as the import's decoder returns it.
func document(t *testing.T) map[string]any {
	t.Helper()
	return decode(t, `{
	  "openapi": "3.1.0",
	  "info": {"title": "shop", "version": "1"},
	  "paths": {
	    "/orders": {
	      "get": {"operationId": "listOrders", "x-apiward-access": "deny"},
	      "post": {"operationId": "createOrder", "x-apiward-max-bytes": 4096,
	        "x-apiward-rate": {"algorithm": "sliding-window", "count": 10, "window_seconds": 60, "key": "subject"}}
	    },
	    "/orders/{id}": {"get": {"operationId": "getOrder"}},
	    "/it's": {"get": {"operationId": "quote'd"}}
	  },
	  "x-apiward-migration": {"version": 1, "items": [
	    {"id": "openapi:operation:GET /orders/{id}", "class": "approximated", "direction": "looser", "accepted": false},
	    {"id": "openapi:operation:GET /orders", "class": "approximated", "direction": "stricter", "accepted": false}
	  ]}
	}`)
}

func decode(t *testing.T, text string) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	var value map[string]any
	if err := decoder.Decode(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

// decisions builds a decisions file around the given actions.
func decisions(t *testing.T, actions string) map[string]any {
	t.Helper()
	return decode(t, `{"overlay": "1.2.0", "info": {"title": "decisions", "version": "1"}, "actions": `+actions+`}`)
}

// apply parses and applies a decisions file to a fresh document.
func apply(t *testing.T, root map[string]any) (map[string]any, []string, error) {
	t.Helper()
	d, err := Parse(root)
	if err != nil {
		return nil, nil, err
	}
	return d.Apply(document(t), DefaultBytes)
}

// DefaultBytes stands for the body limit an operation gets without x-apiward-max-bytes.
const DefaultBytes = 16384

func operation(t *testing.T, doc map[string]any, path, method string) map[string]any {
	t.Helper()
	return doc["paths"].(map[string]any)[path].(map[string]any)[method].(map[string]any)
}

func TestOverlayProfile(t *testing.T) {
	refused := map[string]string{
		"version":       `{"overlay": "1.1.0", "info": {"title": "d", "version": "1"}, "actions": [{"target": "$['paths']", "update": {}}]}`,
		"no info":       `{"overlay": "1.2.0", "actions": [{"target": "$['paths']", "update": {}}]}`,
		"no actions":    `{"overlay": "1.2.0", "info": {"title": "d", "version": "1"}, "actions": []}`,
		"components":    `{"overlay": "1.2.0", "info": {"title": "d", "version": "1"}, "components": {}, "actions": [{"target": "$['paths']", "update": {}}]}`,
		"extension":     `{"overlay": "1.2.0", "info": {"title": "d", "version": "1"}, "x-tool": 1, "actions": [{"target": "$['paths']", "update": {}}]}`,
		"info member":   `{"overlay": "1.2.0", "info": {"title": "d", "version": "1", "x-a": 1}, "actions": [{"target": "$['paths']", "update": {}}]}`,
		"remove":        `{"overlay": "1.2.0", "info": {"title": "d", "version": "1"}, "actions": [{"target": "$['paths']['/orders']['get']", "remove": true}]}`,
		"copy":          `{"overlay": "1.2.0", "info": {"title": "d", "version": "1"}, "actions": [{"target": "$['paths']['/orders']['get']", "copy": "$['paths']"}]}`,
		"no update":     `{"overlay": "1.2.0", "info": {"title": "d", "version": "1"}, "actions": [{"target": "$['paths']['/orders']['get']"}]}`,
		"reference":     `{"overlay": "1.2.0", "info": {"title": "d", "version": "1"}, "actions": [{"$ref": "#/components/actions/a"}]}`,
		"recursive":     `[{"target": "$..get", "update": {}}]`,
		"dot names":     `[{"target": "$.paths['/orders'].get", "update": {}}]`,
		"info":          `[{"target": "$['info']", "update": {"title": "x"}}]`,
		"path item":     `[{"target": "$['paths']['/orders']", "update": {"x-a": 1}}]`,
		"not a method":  `[{"target": "$['paths']['/orders']['parameters']", "update": []}]`,
		"double quotes": `[{"target": "$.paths[*][?@.operationId==\"getOrder\"]", "update": {}}]`,
		"spaces":        `[{"target": "$.paths[*][?@.operationId == 'getOrder']", "update": {}}]`,
		"leading zero":  `[{"target": "$['x-apiward-migration']['items'][01]", "update": {"accepted": true, "direction": "looser"}}]`,
		"item member":   `[{"target": "$['x-apiward-migration']['items'][0]['accepted']", "update": true}]`,
		"ledger root":   `[{"target": "$['x-apiward-migration']", "update": {"version": 1}}]`,
		"bad escape":    `[{"target": "$.paths[*][?@.operationId=='a\\q']", "update": {}}]`,
		"unclosed":      `[{"target": "$['paths']['/orders'", "update": {}}]`,
		"target type":   `[{"target": 1, "update": {}}]`,
		"action member": `[{"target": "$['paths']['/orders']['get']", "update": {}, "x-note": 1}]`,
	}
	for name, text := range refused {
		var root map[string]any
		if strings.HasPrefix(text, "[") {
			root = decisions(t, text)
		} else {
			root = decode(t, text)
		}
		if _, err := Parse(root); err == nil {
			t.Errorf("%s: parsed, want refused", name)
		}
	}
	allowed := decisions(t, `[
	  {"target": "$['paths']['/orders']['post']['x-apiward-rate']", "description": "tighter", "update": {"count": 5}},
	  {"target": "$.paths[*][?@.operationId=='getOrder']", "update": {"x-apiward-access": "deny"}},
	  {"target": "$.paths[*][?@.operationId=='quote\\'d']", "update": {"x-apiward-max-bytes": 100}},
	  {"target": "$['paths']['/orders']['post']['x-apiward-max-bytes']", "update": 1024}
	]`)
	allowed["$self"], allowed["extends"] = "https://example.com/decisions.json", "shop.exchange.json"
	out, stale, err := apply(t, allowed)
	if err != nil || len(stale) != 0 {
		t.Fatalf("stale %v, err %v", stale, err)
	}
	post := operation(t, out, "/orders", "post")
	if rate := post["x-apiward-rate"].(map[string]any); rate["count"] != json.Number("5") || rate["window_seconds"] != json.Number("60") {
		t.Errorf("rate %v", rate)
	}
	if post["x-apiward-max-bytes"] != json.Number("1024") || operation(t, out, "/orders/{id}", "get")["x-apiward-access"] != "deny" ||
		operation(t, out, "/it's", "get")["x-apiward-max-bytes"] != json.Number("100") {
		t.Errorf("document %v", out["paths"])
	}
}

// TestOverlayInputUnchanged applies to a copy: the caller's document stays as it was.
func TestOverlayInputUnchanged(t *testing.T) {
	d, err := Parse(decisions(t, `[{"target": "$['paths']['/orders']['post']['x-apiward-rate']", "update": {"count": 5}}]`))
	if err != nil {
		t.Fatal(err)
	}
	in := document(t)
	if _, _, err := d.Apply(in, DefaultBytes); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, document(t)) {
		t.Fatal("the input document was changed")
	}
}

func TestOverlayStale(t *testing.T) {
	out, stale, err := apply(t, decisions(t, `[
	  {"target": "$['paths']['/gone']['get']", "update": {"x-apiward-access": "deny"}},
	  {"target": "$.paths[*][?@.operationId=='gone']", "update": {"x-apiward-access": "deny"}},
	  {"target": "$['x-apiward-migration'].items[?@.id=='openapi:operation:GET /gone']", "update": {"accepted": true, "direction": "looser"}},
	  {"target": "$['paths']['/orders']['get']['x-apiward-rate']", "update": {"count": 1}}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 4 || !strings.HasPrefix(stale[0], "actions[0]: $['paths']['/gone']") || !strings.HasPrefix(stale[3], "actions[3]: ") {
		t.Fatalf("stale %q", stale)
	}
	if !reflect.DeepEqual(out, document(t)) {
		t.Fatal("stale actions changed the document")
	}
}

func TestOverlayMulti(t *testing.T) {
	root := document(t)
	operation(t, root, "/orders/{id}", "get")["operationId"] = "listOrders"
	d, err := Parse(decisions(t, `[{"target": "$.paths[*][?@.operationId=='listOrders']", "update": {"x-apiward-access": "deny"}}]`))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.Apply(root, DefaultBytes); err == nil || !strings.Contains(err.Error(), "2 nodes") {
		t.Fatalf("err %v, want two matches refused", err)
	}
}

// TestLooserNeedsAccept: a ledger item is accepted only with direction looser
// in the same action, only when it is a looser approximation, and no
// decision on an operation may widen it.
func TestLooserNeedsAccept(t *testing.T) {
	item := "$['x-apiward-migration'].items[?@.id=='openapi:operation:GET /orders/{id}']"
	refused := map[string]string{
		"accept alone":       `[{"target": "` + item + `", "update": {"accepted": true}}]`,
		"direction alone":    `[{"target": "` + item + `", "update": {"direction": "looser"}}]`,
		"other member":       `[{"target": "` + item + `", "update": {"accepted": true, "direction": "looser", "class": "exact"}}]`,
		"stricter item":      `[{"target": "$['x-apiward-migration']['items'][1]", "update": {"accepted": true, "direction": "looser"}}]`,
		"allow a denied":     `[{"target": "$['paths']['/orders']['get']", "update": {"x-apiward-access": "allow"}}]`,
		"allow by default":   `[{"target": "$['paths']['/orders/{id}']['get']", "update": {"x-apiward-access": "allow"}}]`,
		"higher count":       `[{"target": "$['paths']['/orders']['post']['x-apiward-rate']", "update": {"count": 11}}]`,
		"shorter window":     `[{"target": "$['paths']['/orders']['post']['x-apiward-rate']", "update": {"window_seconds": 59}}]`,
		"other key":          `[{"target": "$['paths']['/orders']['post']['x-apiward-rate']", "update": {"key": "client_address"}}]`,
		"rate as text":       `[{"target": "$['paths']['/orders']['post']['x-apiward-rate']", "update": {"count": "5"}}]`,
		"larger body":        `[{"target": "$['paths']['/orders']['post']", "update": {"x-apiward-max-bytes": 4097}}]`,
		"above the default":  `[{"target": "$['paths']['/orders/{id}']['get']", "update": {"x-apiward-max-bytes": 16385}}]`,
		"roles":              `[{"target": "$['paths']['/orders/{id}']['get']", "update": {"x-apiward-roles": ["admin"]}}]`,
		"public":             `[{"target": "$['paths']['/orders/{id}']['get']", "update": {"security": []}}]`,
		"target":             `[{"target": "$['paths']['/orders/{id}']['get']", "update": {"x-apiward-target": "/orders/*"}}]`,
		"type change":        `[{"target": "$['paths']['/orders']['post']['x-apiward-rate']", "update": 5}]`,
		"rate replaced bare": `[{"target": "$['paths']['/orders']['post']['x-apiward-rate']['count']", "update": {"n": 1}}]`,
	}
	for name, actions := range refused {
		if _, _, err := apply(t, decisions(t, actions)); err == nil {
			t.Errorf("%s: applied, want refused", name)
		}
	}
	out, _, err := apply(t, decisions(t, `[{"target": "`+item+`", "update": {"accepted": true, "direction": "looser"}},
	  {"target": "$['paths']['/orders/{id}']['get']", "update": {"x-apiward-rate":
	    {"algorithm": "sliding-window", "count": 100, "window_seconds": 60, "key": "subject"}}}]`))
	if err != nil {
		t.Fatal(err)
	}
	items := out[exchange.Member].(map[string]any)["items"].([]any)
	if first := items[0].(map[string]any); first["accepted"] != true || first["direction"] != "looser" || first["class"] != "approximated" {
		t.Fatalf("item %v", first)
	}
}

// TestOverlayOrder applies actions in order: each sees the result of the one before.
func TestOverlayOrder(t *testing.T) {
	rate := "$['paths']['/orders/{id}']['get']['x-apiward-rate']"
	add := `{"target": "$['paths']['/orders/{id}']['get']", "update": {"x-apiward-rate":
	  {"algorithm": "sliding-window", "count": 50, "window_seconds": 60, "key": "subject"}}}`
	out, stale, err := apply(t, decisions(t, `[`+add+`, {"target": "`+rate+`", "update": {"count": 40}}]`))
	if err != nil || len(stale) != 0 {
		t.Fatalf("stale %v, err %v", stale, err)
	}
	if count := operation(t, out, "/orders/{id}", "get")["x-apiward-rate"].(map[string]any)["count"]; count != json.Number("40") {
		t.Fatalf("count %v, want 40", count)
	}
	if _, stale, _ := apply(t, decisions(t, `[{"target": "`+rate+`", "update": {"count": 40}}, `+add+`]`)); len(stale) != 1 {
		t.Fatalf("stale %v, want the first action stale", stale)
	}
	lower := `{"target": "` + rate + `", "update": {"count": 30}}`
	higher := `{"target": "` + rate + `", "update": {"count": 45}}`
	if _, _, err := apply(t, decisions(t, `[`+add+`, `+lower+`, `+higher+`]`)); err == nil {
		t.Fatal("raising a count lowered by an earlier action was applied")
	}
	if _, _, err := apply(t, decisions(t, `[`+add+`, `+higher+`, `+lower+`]`)); err != nil {
		t.Fatal(err)
	}
}
