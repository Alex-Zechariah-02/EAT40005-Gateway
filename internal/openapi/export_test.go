// Export tests: every mapping rule, both formats, the byte-identical round trip and the flags.

package openapi

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

func policyFixture(t *testing.T, name string) policy.Policy {
	t.Helper()
	p, err := policy.DecodePolicy(fixture(t, name))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return p
}

// exported returns the description of one API, re-read through the import's gate.
func exported(t *testing.T, p policy.Policy, options ExportOptions, format Format) (map[string]any, *ExportReport, []byte) {
	t.Helper()
	document, report, err := Export(p, options)
	if err != nil {
		t.Fatal(err)
	}
	data, err := encodeDocument(document, format)
	if err != nil {
		t.Fatal(err)
	}
	value, err := decodeSpec(data, format)
	if err != nil {
		t.Fatalf("the import cannot read the export: %v\n%s", err, data)
	}
	return value.(map[string]any), report, data
}

// at walks a decoded description by member names and list indexes.
func at(t *testing.T, value any, path ...any) any {
	t.Helper()
	for _, step := range path {
		switch key := step.(type) {
		case string:
			value = value.(map[string]any)[key]
		case int:
			value = value.([]any)[key]
		}
	}
	return value
}

func TestExportOpenAPI(t *testing.T) {
	shop := policyFixture(t, "policy-shop.json")
	for _, format := range []Format{JSON, YAML} {
		t.Run(string(format), func(t *testing.T) {
			doc, report, _ := exported(t, shop, ExportOptions{API: "shop"}, format)
			expect := func(want any, path ...any) {
				t.Helper()
				if got := at(t, doc, path...); !reflect.DeepEqual(generic(t, got), generic(t, want)) {
					t.Errorf("%v = %#v, want %#v", path, got, want)
				}
			}
			expect("3.1.0", "openapi")
			expect(map[string]any{"type": "http", "scheme": "bearer", "bearerFormat": "JWT"}, "components", "securitySchemes", "apiward")
			if paths := sortedKeys(doc["paths"].(map[string]any)); strings.Join(paths, " ") != "/health/ /items /items/{rest} /private" {
				t.Errorf("paths %v", paths)
			}
			item := at(t, doc, "paths", "/items/{rest}", "get")
			expect("item", "paths", "/items/{rest}", "get", "operationId")
			expect("/items/*", "paths", "/items/{rest}", "get", "x-apiward-target")
			expect([]any{map[string]any{"apiward": []any{}}}, "paths", "/items/{rest}", "get", "security")
			expect([]string{"reader", "writer"}, "paths", "/items/{rest}", "get", "x-apiward-roles")
			expect(map[string]any{"algorithm": "sliding-window", "count": 60, "window_seconds": 60, "key": "subject"}, "paths", "/items/{rest}", "get", "x-apiward-rate")
			expect(map[string]any{"name": "rest", "in": "path", "required": true, "schema": map[string]any{"type": "string"}}, "paths", "/items/{rest}", "get", "parameters", 0)
			expect("allow", "paths", "/items/{rest}", "get", "x-apiward-access")
			if _, present := item.(map[string]any)["requestBody"]; present {
				t.Error("an operation without a body rule has a requestBody")
			}
			expect([]any{}, "paths", "/items", "get", "security")
			expect(nil, "paths", "/items", "get", "x-apiward-roles")
			expect("deny", "paths", "/items/{rest}", "delete", "x-apiward-access")
			expect("deny", "paths", "/items", "options", "x-apiward-access")
			expect([]string{}, "paths", "/private", "put", "x-apiward-roles")
			expect(map[string]any{"name": "X-Locale", "in": "header", "schema": map[string]any{"type": "string", "enum": []string{"en", "ms"}}}, "paths", "/items/{rest}", "patch", "parameters", 1)
			expect(map[string]any{"name": "X-Tenant", "in": "header", "required": true, "schema": map[string]any{"type": "string", "maxLength": 32}}, "paths", "/items/{rest}", "patch", "parameters", 2)
			expect(true, "paths", "/items/{rest}", "patch", "requestBody", "required")
			expect(512, "paths", "/items/{rest}", "patch", "x-apiward-max-bytes")
			if got := at(t, doc, "paths", "/items/{rest}", "patch", "requestBody", "content", "application/json", "schema", "properties", "price", "maximum"); got != json.Number("1e6") {
				t.Errorf("number literal %v, want 1e6 unchanged", got)
			}
			expect(nil, "servers")
			if len(report.Exported) != 8 || report.Operations != 8 || report.Exit() != ExitPartial {
				t.Errorf("report %+v", report)
			}
			for _, changed := range report.Changed {
				if changed.Field != "target" || !strings.Contains(changed.Note, "deeper paths under /items/") {
					t.Errorf("unexpected change %+v", changed)
				}
			}
			if len(report.Changed) != 3 {
				t.Errorf("changed %+v, want the three /items/* operations", report.Changed)
			}
		})
	}
	t.Run("root prefix", func(t *testing.T) {
		doc, _, _ := exported(t, shop, ExportOptions{API: "audit"}, JSON)
		if at(t, doc, "paths", "/{rest}", "get", "x-apiward-target") != "/*" {
			t.Fatalf("paths %v", doc["paths"])
		}
		if _, present := doc["components"]; !present {
			t.Fatal("signed-in API without a security scheme")
		}
	})
	t.Run("public API has no security scheme", func(t *testing.T) {
		doc, report, _ := exported(t, policyFixture(t, "policy-public.json"), ExportOptions{API: "site"}, JSON)
		if _, present := doc["components"]; present {
			t.Fatal("security scheme written for a public API")
		}
		if _, present := at(t, doc, "paths", "/search", "get").(map[string]any)["x-apiward-roles"]; present {
			t.Fatal("roles written for a public operation")
		}
		if report.Exit() != ExitPartial || len(report.Changed) != 1 {
			t.Fatalf("report %+v", report)
		}
	})
	t.Run("what cannot be described", func(t *testing.T) {
		p := policyFixture(t, "policy-public.json")
		p.Operations = append(p.Operations,
			policy.Operation{ID: "home2", API: "site", Method: "PUT", Target: "/", Access: "allow", Auth: "none"},
			policy.Operation{ID: "site.braces", API: "site", Method: "GET", Target: "/x/{id}", Access: "allow", Auth: "none"},
			policy.Operation{ID: "site.closed", API: "site", Method: "GET", Target: "/closed", Access: "allow", Auth: "none",
				Headers: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"X-Kept":{"type":"string"}}}`)})
		doc, report, _ := exported(t, p, ExportOptions{API: "site"}, JSON)
		if at(t, doc, "paths", "/", "put", "operationId") != "home2" {
			t.Fatal("operation without the API prefix not exported")
		}
		if _, present := doc["paths"].(map[string]any)["/x/{id}"]; present {
			t.Fatal("a target with braces was exported as a template")
		}
		var fields []string
		for _, changed := range report.Changed {
			fields = append(fields, changed.Operation+" "+changed.Field)
		}
		if at(t, doc, "paths", "/closed", "get", "parameters", 0, "name") != "X-Kept" {
			t.Fatal("header of a closed header schema not exported")
		}
		want := "site.pages target,home2 id,site.braces target"
		if strings.Join(fields, ",") != want {
			t.Fatalf("changed %v, want %s", fields, want)
		}
		if len(report.Exported) != 6 || report.Operations != 7 {
			t.Fatalf("report %+v", report)
		}
	})
	t.Run("refusals", func(t *testing.T) {
		if _, _, err := Export(shop, ExportOptions{API: "missing"}); err == nil {
			t.Fatal("unknown API exported")
		}
		broken := shop
		broken.Permits = append(broken.Permits, policy.Permit{Role: "nobody", Operation: "shop.list"})
		if _, _, err := Export(broken, ExportOptions{API: "shop"}); err == nil {
			t.Fatal("a policy the compiler refuses was exported")
		}
	})
}

func TestRoundTripPolicy(t *testing.T) {
	for _, name := range []string{"policy-shop.json", "policy-public.json"} {
		p := policyFixture(t, name)
		want, err := policy.Encode(p)
		if err != nil {
			t.Fatal(err)
		}
		for _, api := range p.APIs {
			for _, format := range []Format{JSON, YAML} {
				t.Run(name+"/"+api.ID+"/"+string(format), func(t *testing.T) {
					_, _, data := exported(t, p, ExportOptions{API: api.ID, WithUpstream: true}, format)
					result, report, err := Import(data, format, Options{API: api.ID, Upstream: api.Upstream, Base: p,
						MaxBodyBytes: DefaultBodyBytes, WithPermits: true})
					if err != nil {
						t.Fatal(err)
					}
					got, err := policy.Encode(result)
					if err != nil {
						t.Fatal(err)
					}
					if string(got) != string(want) {
						t.Fatalf("round trip differs:\n%s\nwant:\n%s", got, want)
					}
					if len(report.Widened) != 0 || len(report.Refused) != 0 || len(report.NeedsPermissions) != 0 {
						t.Fatalf("report %+v", report)
					}
				})
			}
		}
	}
}

func TestExportFlags(t *testing.T) {
	shop := policyFixture(t, "policy-shop.json")
	_, report, without := exported(t, shop, ExportOptions{API: "shop"}, YAML)
	if strings.Contains(string(without), "9201") || strings.Contains(string(without), "servers") {
		t.Fatalf("upstream written without --with-upstream:\n%s", without)
	}
	if len(report.Omitted) != 1 || !strings.Contains(report.Omitted[0], "--with-upstream") {
		t.Fatalf("omitted %v", report.Omitted)
	}
	doc, report, _ := exported(t, shop, ExportOptions{API: "shop", WithUpstream: true}, JSON)
	want := generic(t, []any{map[string]any{"url": "http://127.0.0.1:9201", "description": "shop-svc"}})
	if !reflect.DeepEqual(doc["servers"], want) || len(report.Omitted) != 0 {
		t.Fatalf("servers %v, omitted %v", doc["servers"], report.Omitted)
	}
}
