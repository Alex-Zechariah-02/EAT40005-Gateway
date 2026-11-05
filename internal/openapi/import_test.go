// Mapping tests: every mapping row, every refusal code, the report format and exit codes.

package openapi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

// testBase holds an orders API to be replaced and an inventory API to keep.
func testBase() policy.Policy {
	return policy.Policy{
		SchemaVersion: policy.SchemaVersion,
		Profile:       policy.ProfileCore,
		Upstreams:     []policy.Upstream{{ID: "orders-svc", Dial: "127.0.0.1:9101"}, {ID: "stock-svc", Dial: "127.0.0.1:9102"}},
		APIs:          []policy.API{{ID: "orders", Upstream: "orders-svc"}, {ID: "inventory", Upstream: "stock-svc"}},
		Operations: []policy.Operation{
			{ID: "orders.old", API: "orders", Method: "GET", Target: "/old", Access: "allow", Auth: "required"},
			{ID: "inventory.list", API: "inventory", Method: "GET", Target: "/inventory", Access: "allow", Auth: "required"},
		},
		Roles:   []policy.Role{{ID: "customer"}},
		Permits: []policy.Permit{{Role: "customer", Operation: "orders.old"}, {Role: "customer", Operation: "inventory.list"}},
	}
}

func testOptions() Options {
	return Options{API: "orders", Upstream: "orders-svc", Base: testBase(), MaxBodyBytes: DefaultBodyBytes}
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// generic re-reads JSON-encodable data as maps and slices for comparison.
func generic(t *testing.T, value any) any {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestImportOpenAPI(t *testing.T) {
	t.Run("orders report", func(t *testing.T) {
		_, report, err := Import(fixture(t, "orders.json"), JSON, testOptions())
		if err != nil {
			t.Fatal(err)
		}
		var want any
		if err := json.Unmarshal(fixture(t, "orders.report.json"), &want); err != nil {
			t.Fatal(err)
		}
		if got := generic(t, report); !reflect.DeepEqual(got, want) {
			gotJSON, _ := json.MarshalIndent(got, "", "  ")
			t.Fatalf("report differs; got:\n%s", gotJSON)
		}
		if report.Exit() != ExitPartial {
			t.Fatalf("exit %d, want %d", report.Exit(), ExitPartial)
		}
	})

	t.Run("orders policy", func(t *testing.T) {
		out, _, err := Import(fixture(t, "orders.json"), JSON, testOptions())
		if err != nil {
			t.Fatal(err)
		}
		want := []struct{ id, method, target, auth, headers, body string }{
			{"inventory.list", "GET", "/inventory", "required", "", ""},
			{"orders.health", "GET", "/health", "none", "", ""},
			{"orders.listOrders", "GET", "/orders", "required",
				`{"type":"object","additionalProperties":true,"required":["X-Tenant"],"properties":{"X-Tenant":{"type":"string","maxLength":32}}}`, ""},
			{"orders.createOrder", "POST", "/orders", "required", "",
				`{"type":"object","additionalProperties":false,"required":["item","quantity"],"properties":{"item":{"type":"string","minLength":1,"maxLength":16},"quantity":{"type":"integer","minimum":1,"maximum":40},"note":{"type":["string","null"],"maxLength":200}}}`},
			{"orders.getOrder", "GET", "/orders/*", "required", "", ""},
			{"orders.updateOrder", "PATCH", "/orders/*", "required", "",
				`{"type":"object","additionalProperties":true,"properties":{"quantity":{"type":"integer","minimum":1,"maximum":40}}}`},
			{"orders.listStock", "GET", "/stock", "required", "", ""},
		}
		if len(out.Operations) != len(want) {
			t.Fatalf("got %d operations, want %d", len(out.Operations), len(want))
		}
		for i, w := range want {
			op := out.Operations[i]
			if op.ID != w.id || op.Method != w.method || op.Target != w.target || op.Auth != w.auth || op.Access != "allow" || op.Rate != nil {
				t.Fatalf("operation %d = %+v, want %+v", i, op, w)
			}
			sameJSON(t, w.id+" headers", op.Headers, w.headers)
			if (op.Body == nil) != (w.body == "") {
				t.Fatalf("%s body = %v, want %q", w.id, op.Body, w.body)
			}
			if op.Body != nil {
				sameJSON(t, w.id+" body", op.Body.Schema, w.body)
				if op.Body.MaxBytes != DefaultBodyBytes {
					t.Fatalf("%s max_bytes %d", w.id, op.Body.MaxBytes)
				}
			}
		}
		wantAPIs := []policy.API{{ID: "inventory", Upstream: "stock-svc"}, {ID: "orders", Upstream: "orders-svc"}}
		wantPermits := []policy.Permit{{Role: "customer", Operation: "inventory.list"}}
		if !reflect.DeepEqual(out.APIs, wantAPIs) || !reflect.DeepEqual(out.Permits, wantPermits) {
			t.Fatalf("apis %+v permits %+v", out.APIs, out.Permits)
		}
		if !reflect.DeepEqual(out.Upstreams, testBase().Upstreams) || !reflect.DeepEqual(out.Roles, testBase().Roles) {
			t.Fatal("upstreams or roles changed")
		}
	})

	t.Run("refusal codes", func(t *testing.T) {
		_, report, err := Import(fixture(t, "refusals.json"), JSON, testOptions())
		if err != nil {
			t.Fatal(err)
		}
		const body = "/requestBody/content/application~1json/schema"
		want := map[string][2]string{
			"GET /a/{x}":          {CodeTargetCollision, ""},
			"GET /a/{y}":          {CodeTargetCollision, ""},
			"GET /bad-id":         {CodeOperationID, ""},
			"GET /no-id":          {CodeOperationID, ""},
			"GET /dup1":           {CodeDuplicateID, ""},
			"GET /dup2":           {CodeDuplicateID, ""},
			"GET /apikey":         {CodeSecurity, ""},
			"GET /basic":          {CodeSecurity, ""},
			"GET /optional":       {CodeSecurity, ""},
			"GET /unknown-scheme": {CodeSecurity, ""},
			"POST /form":          {CodeMediaType, ""},
			"POST /cycle":         {CodeCycle, "#/components/schemas/Tree/properties/child"},
			"POST /external":      {CodeExternalRef, "#/paths/~1external/post" + body},
			"POST /missing":       {CodeRefInvalid, "#/paths/~1missing/post" + body},
			"POST /format":        {CodeKeyword, "#/paths/~1format/post" + body + "/properties/email/format"},
			"POST /extra":         {CodeKeyword, "#/paths/~1extra/post" + body + "/additionalProperties"},
			"POST /sibling":       {CodeKeyword, "#/paths/~1sibling/post" + body + "/maxProperties"},
			"POST /fanout":        {CodeSchemaLimit, "#/components/schemas/Fan4/properties/p7"},
			"GET /count":          {CodeCompiler, "headers.properties.X-Count.type"},
			"GET /content-header": {CodeKeyword, "#/paths/~1content-header/get/parameters/0/content"},
			"POST /untyped":       {CodeCompiler, "body.schema.type"},
		}
		got := map[string][2]string{}
		for _, r := range report.Refused {
			got[r.Method+" "+r.Path] = [2]string{r.Code, r.Field}
			if r.Message == "" {
				t.Errorf("%s %s has no message", r.Method, r.Path)
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("refused:\n got %v\nwant %v", got, want)
		}
		if len(report.Mapped)+len(report.Widened) != 0 || report.OperationsInSpec != len(want) {
			t.Fatalf("mapped %d widened %d in spec %d", len(report.Mapped), len(report.Widened), report.OperationsInSpec)
		}
		for _, r := range report.Refused {
			if r.Path == "/bad-id" && r.Suggestion != "list-orders" || r.Path == "/no-id" && r.Suggestion != "get-no-id" {
				t.Errorf("%s suggestion %q", r.Path, r.Suggestion)
			}
		}
	})

	t.Run("everything mapped", func(t *testing.T) {
		spec := `{"openapi":"3.0.3","info":{"title":"t","version":"1"},"security":[{"bearer":[]}],
			"paths":{"/orders":{"get":{"operationId":"list"}}},
			"components":{"securitySchemes":{"bearer":{"type":"http","scheme":"Bearer"}}}}`
		_, report, err := Import([]byte(spec), JSON, testOptions())
		if err != nil {
			t.Fatal(err)
		}
		if report.Exit() != ExitComplete || len(report.Mapped) != 1 || report.Mapped[0].Note != "" {
			t.Fatalf("exit %d, mapped %+v", report.Exit(), report.Mapped)
		}
	})

	t.Run("widened alone exits 2", func(t *testing.T) {
		spec := `{"openapi":"3.1.0","info":{"title":"t","version":"1"},"paths":{"/orders/{id}":{"get":{"operationId":"get"}}}}`
		_, report, err := Import([]byte(spec), JSON, testOptions())
		if err != nil {
			t.Fatal(err)
		}
		if report.Exit() != ExitPartial || len(report.Widened) != 1 || len(report.Refused) != 0 {
			t.Fatalf("exit %d, widened %+v", report.Exit(), report.Widened)
		}
	})

	t.Run("whole file refused", func(t *testing.T) {
		valid := `{"openapi":"3.1.0","info":{"title":"t","version":"1"},"paths":{}}`
		cases := []struct {
			name, spec string
			change     func(*Options)
			want       string
		}{
			{"swagger 2", `{"swagger":"2.0","paths":{}}`, nil, "openapi"},
			{"version 4", `{"openapi":"4.0.0","paths":{}}`, nil, "openapi"},
			{"not an object", `[1]`, nil, "object"},
			{"malformed", `{"openapi":`, nil, "malformed_json"},
			{"duplicate member", `{"openapi":"3.1.0","openapi":"3.1.0"}`, nil, "duplicate_member"},
			{"too deep", `{"openapi":"3.1.0","x":` + strings.Repeat(`[`, 16) + strings.Repeat(`]`, 16) + `}`, nil, "limit_exceeded"},
			{"unknown upstream", valid, func(o *Options) { o.Upstream = "nowhere" }, "upstream"},
			{"invalid api", valid, func(o *Options) { o.API = "bad api" }, "api"},
			{"body limit", valid, func(o *Options) { o.MaxBodyBytes = policy.MaxBodyLimit + 1 }, "max-body-bytes"},
		}
		for _, c := range cases {
			options := testOptions()
			if c.change != nil {
				c.change(&options)
			}
			_, _, err := Import([]byte(c.spec), JSON, options)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s: error %v, want it to mention %q", c.name, err, c.want)
			}
		}
	})
}

func sameJSON(t *testing.T, name string, got json.RawMessage, want string) {
	t.Helper()
	if want == "" {
		if len(got) != 0 {
			t.Fatalf("%s = %s, want none", name, got)
		}
		return
	}
	var g, w any
	if json.Unmarshal(got, &g) != nil || json.Unmarshal([]byte(want), &w) != nil || !reflect.DeepEqual(g, w) {
		t.Fatalf("%s = %s, want %s", name, got, want)
	}
}
