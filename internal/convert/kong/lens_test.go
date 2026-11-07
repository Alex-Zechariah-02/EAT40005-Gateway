// Tests of the decK writer: the two lens laws, routes written from operations, and what each flag unlocks.

package kong

import (
	"strings"
	"testing"

	"github.com/Alex-Zechariah-02/apiward/internal/exchange"
	"github.com/Alex-Zechariah-02/apiward/internal/openapi"
)

// stored passes a result through the exchange file as written and read, so
// the writer sees what migrate export sees.
func stored(t *testing.T, result *Result) (map[string]any, *exchange.Ledger) {
	t.Helper()
	attached, err := exchange.Attach(result.Document, result.Ledger)
	if err != nil {
		t.Fatal(err)
	}
	data, err := openapi.Encode(attached, openapi.JSON)
	if err != nil {
		t.Fatal(err)
	}
	value, err := openapi.Decode(data, openapi.JSON, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	document, ledger, err := exchange.Split(value.(map[string]any))
	if err != nil {
		t.Fatal(err)
	}
	return document, ledger
}

// TestLensGetPut: Kong → exchange → Kong without edits gives the original
// API configuration back; consumers are not part of it.
func TestLensGetPut(t *testing.T) {
	root, _ := fixture(t, "clean.yaml")
	document, ledger := stored(t, read(t, "clean.yaml"))
	out, report, err := Write(document, ledger, Options{WithUpstream: true})
	if err != nil {
		t.Fatal(err)
	}
	delete(root, "consumers")
	if !same(out, root) {
		t.Errorf("round trip changed the file:\n got %s\nwant %s", encode(t, out), encode(t, root))
	}
	if len(report.Generated) != 0 || len(report.Changed) != 0 || len(report.Restored) != 4 {
		t.Errorf("report = %+v", report)
	}
	if len(report.ManualSteps) != 2 { // consumers, trusted issuers
		t.Errorf("manual steps = %v", report.ManualSteps)
	}
}

// reread reads an exported decK file back as migrate import would.
func reread(t *testing.T, deck map[string]any) *Result {
	t.Helper()
	data, err := openapi.Encode(deck, openapi.YAML)
	if err != nil {
		t.Fatal(err)
	}
	value, err := openapi.Decode(data, openapi.YAML, 8<<20)
	if err != nil {
		t.Fatalf("exported file refused by the gate: %v", err)
	}
	result, err := Read(value.(map[string]any), data)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// TestLensPutGet: an edit on the gateway side survives export and import;
// the edited route is written from its operations, the others as read.
func TestLensPutGet(t *testing.T) {
	document, ledger := stored(t, read(t, "clean.yaml"))
	object(object(object(object(document["paths"])["/orders"])["get"])["x-apiward-rate"])["count"] = int64(5)
	out, report, err := Write(document, ledger, Options{WithUpstream: true, WithTrust: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Generated) != 4 || len(report.Restored) != 3 || report.Exit() != 2 {
		t.Fatalf("report = %+v", report)
	}
	paths := reread(t, out).Document["paths"].(map[string]any)
	for path, methods := range map[string][]string{"/orders": {"get", "post"}, "/orders/{rest}": {"get", "post"}} {
		for _, method := range methods {
			op := object(object(paths[path])[method])
			want := `{"algorithm":"sliding-window","count":20,"key":"subject","window_seconds":60}`
			if path == "/orders" && method == "get" {
				want = `{"algorithm":"sliding-window","count":5,"key":"subject","window_seconds":60}`
			}
			if got := encode(t, op["x-apiward-rate"]); got != want {
				t.Errorf("%s %s rate = %s, want %s", method, path, got, want)
			}
			if encode(t, op["security"]) != `[{"apiward":[]}]` || encode(t, op["x-apiward-roles"]) != `["CUSTOMER"]` {
				t.Errorf("%s %s lost sign-in or roles: %s %s", method, path, encode(t, op["security"]), encode(t, op["x-apiward-roles"]))
			}
		}
	}
	if object(object(paths["/orders"])["post"])["x-apiward-max-bytes"] == nil {
		t.Error("the body limit of POST /orders was lost")
	}
}

// TestKongExport checks routes written from operations of another source:
// anchored paths, fail-closed plugins, and what is not carried.
func TestKongExport(t *testing.T) {
	op := func(extra map[string]any) map[string]any {
		o := map[string]any{"operationId": "orders.get", "x-apiward-access": "allow", "security": []any{map[string]any{"apiward": []any{}}}}
		for k, v := range extra {
			o[k] = v
		}
		return o
	}
	document := map[string]any{"paths": map[string]any{
		"/orders": map[string]any{"get": op(map[string]any{"x-apiward-roles": []any{"STAFF"},
			"x-apiward-rate": map[string]any{"algorithm": "sliding-window", "count": int64(9), "window_seconds": int64(60), "key": "subject"}})},
		"/orders/{rest}": map[string]any{"get": op(map[string]any{"operationId": "deep.get", "x-apiward-target": "/orders/*", "x-apiward-roles": []any{}})},
		"/odd": map[string]any{"post": op(map[string]any{"operationId": "odd.post", "security": []any{},
			"x-apiward-rate":      map[string]any{"algorithm": "sliding-window", "count": int64(3), "window_seconds": int64(30), "key": "client_address"},
			"x-apiward-max-bytes": int64(512), "requestBody": map[string]any{"content": map[string]any{"application/json": map[string]any{
				"schema": map[string]any{"type": "object", "additionalProperties": false}}}}})},
	}}
	ledger := exchange.New(exchange.Source{Format: "openapi", FormatVersion: "3.1.0", SHA256: strings.Repeat("0", 64)})
	out, report, err := Write(document, ledger, Options{})
	if err != nil {
		t.Fatal(err)
	}
	routes := list(object(list(out["services"])[0])["routes"])
	want := []string{
		`{"methods":["POST"],"name":"odd-post","paths":["~/odd$"],"plugins":[{"config":{"allowed_payload_size":512,"require_content_length":false,"size_unit":"bytes"},"name":"request-size-limiting"}],"regex_priority":100000,"strip_path":false}`,
		`{"methods":["GET"],"name":"orders-get","paths":["~/orders$"],"plugins":[{"config":{"claims_to_verify":["exp"]},"name":"jwt"},{"config":{"allow":["STAFF"]},"name":"acl"},{"config":{"fault_tolerant":false,"limit_by":"consumer","minute":9,"policy":"local"},"name":"rate-limiting"}],"regex_priority":100000,"strip_path":false}`,
		`{"methods":["GET"],"name":"deep-get","paths":["~/orders/.+"],"plugins":[{"config":{"claims_to_verify":["exp"]},"name":"jwt"},{"config":{"message":"Forbidden","status_code":403},"name":"request-termination"}],"regex_priority":8,"strip_path":false}`,
	}
	if len(routes) != len(want) {
		t.Fatalf("routes = %s", encode(t, routes))
	}
	for i := range want {
		if got := encode(t, routes[i]); got != want[i] {
			t.Errorf("route %d:\n got %s\nwant %s", i, got, want[i])
		}
	}
	notCarried := encode(t, report.NotCarried)
	if !strings.Contains(notCarried, `"field":"rate"`) || !strings.Contains(notCarried, `"field":"body"`) {
		t.Errorf("not carried = %s", notCarried)
	}
	if object(list(out["services"])[0])["host"] != placeholder || out["consumers"] != nil {
		t.Error("an address or consumer written without its flag")
	}
}

// TestExportFlags checks the address and issuer keys only with their flags.
func TestExportFlags(t *testing.T) {
	document, ledger := stored(t, read(t, "clean.yaml"))
	plain, _, err := Write(document, ledger, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if text := encode(t, plain); strings.Contains(text, "orders.internal") || strings.Contains(text, "PUBLIC KEY") {
		t.Errorf("address or key written without its flag: %s", text)
	}
	full, _, err := Write(document, ledger, Options{WithUpstream: true, WithTrust: true})
	if err != nil {
		t.Fatal(err)
	}
	if text := encode(t, full); !strings.Contains(text, "http://orders.internal:8081") || !strings.Contains(text, "BEGIN PUBLIC KEY") {
		t.Errorf("flags did not write the address and key: %s", text)
	}
}
