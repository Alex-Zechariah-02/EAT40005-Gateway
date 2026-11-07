// Tests of the decK reader: one case per mapping row, file refusals, and that every setting has an item.

package kong

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/Alex-Zechariah-02/apiward/internal/exchange"
	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

// TestKongImport checks one ledger item per mapping row of the shop fixture.
func TestKongImport(t *testing.T) {
	result := read(t, "shop.yaml")
	items := map[string]exchange.Item{}
	for _, item := range result.Ledger.Items {
		items[strings.TrimPrefix(item.ID, Format+":")] = item
	}
	cases := []struct {
		id        string
		class     exchange.Class
		direction exchange.Direction
		note      string
	}{
		{"service:shop", exchange.Equivalent, "", "--upstream"},
		{"service:shop:connect_timeout", exchange.NotExpressible, "", "way back"},
		{"operation:items:GET /items", exchange.Approximated, exchange.Stricter, "/itemsx"},
		{"operation:items:GET /items/{rest}", exchange.Approximated, exchange.Stricter, "/itemsx"},
		{"operation:items:GET /items:rate-limiting", exchange.Approximated, exchange.Both, "ceiling 119 ≤ 120, guarantee 30 < 60"},
		{"operation:items:GET /items:fault_tolerant", exchange.Approximated, exchange.Stricter, "refuses requests it cannot check"},
		{"operation:items:GET /items:body", exchange.Approximated, exchange.Stricter, "refuses a request with a body"},
		{"operation:orders:GET /orders/{rest}", exchange.Approximated, exchange.Stricter, "/orders/ itself"},
		{"operation:orders:POST /orders/{rest}:jwt", exchange.Approximated, exchange.Stricter, "audience"},
		{"operation:orders:POST /orders/{rest}:acl", exchange.Approximated, exchange.Stricter, "roles claim"},
		{"operation:orders:POST /orders/{rest}:rate-limiting", exchange.Approximated, exchange.Both, "per consumer"},
		{"operation:orders:POST /orders/{rest}:request-size-limiting", exchange.Equivalent, "", "8192 bytes"},
		{"operation:orders:POST /orders/{rest}:body", exchange.Approximated, exchange.Stricter, "JSON object of at most 8192"},
		{"operation:catalog:GET /catalog:jwt", exchange.Approximated, exchange.Looser, "per-account rate limits become per-address"},
		{"operation:staff:GET /staff:jwt", exchange.Approximated, exchange.Stricter, "refused by the acl"},
		{"operation:burst:GET /burst:rate-limiting", exchange.Approximated, exchange.Looser, "minute are not enforced"},
		{"plugin:route.orders:cors", exchange.NotExpressible, "", "routes differ in browser access"},
		{"plugin:global:prometheus", exchange.NotExpressible, "", "Carried"},
		{"plugin:route.session:session", exchange.NotExpressible, "", "Carried"},
		{"plugin:consumer.guest:rate-limiting", exchange.NotExpressible, "", CodeConsumer},
		{"route:legacy", exchange.NotExpressible, "", CodeTransform},
		{"route:search", exchange.NotExpressible, "", CodeMatch + ": regular-expression"},
		{"operation:hosted:GET /hosted:when", exchange.Equivalent, "", "most conditions"},
		{"route:versions", exchange.NotExpressible, "", CodeMatch + ": a header condition with one value"},
		{"route:keyed", exchange.NotExpressible, "", CodeAuth},
		{"plugin:route.keyed:key-auth", exchange.NotExpressible, "", CodeAuth},
		{"route:denied", exchange.NotExpressible, "", CodeDeny},
		{"plugin:route.denied:jwt", exchange.NotExpressible, "", CodeNotImported},
		{"route:monthly", exchange.NotExpressible, "", CodeRate},
		{"route:odd", exchange.NotExpressible, "", CodeUnknown},
		{"consumer:shop-frontend", exchange.NotExpressible, "", "not migrated"},
		{"certificates:0", exchange.NotExpressible, "", CodeOutOfScope},
		{"upstreams:shop.internal", exchange.NotExpressible, "", CodeOutOfScope},
	}
	for _, c := range cases {
		item, found := items[c.id]
		switch {
		case !found:
			t.Errorf("%s: no item", c.id)
		case item.Class != c.class || item.Direction != c.direction || !strings.Contains(item.Note, c.note):
			t.Errorf("%s: got %s %s %q, want %s %s containing %q", c.id, item.Class, item.Direction, item.Note, c.class, c.direction, c.note)
		}
	}
	if _, found := items["operation:orders:GET /orders/{rest}:fault_tolerant"]; found {
		t.Error("fault_tolerant: false still reported as failing open")
	}
	if _, found := items["operation:orders:GET /orders/{rest}:request-size-limiting"]; found {
		t.Error("a body limit reported on GET, which accepts no body")
	}
	if err := result.Ledger.Check(); err != nil {
		t.Fatal(err)
	}
}

// TestKongOperations checks what the rows write into the operations.
func TestKongOperations(t *testing.T) {
	paths := read(t, "shop.yaml").Document["paths"].(map[string]any)
	op := func(path, method string) map[string]any { return object(object(paths[path])[method]) }
	checks := []struct{ path, method, member, want string }{
		{"/items", "get", "security", `[]`},
		{"/items", "get", "x-apiward-rate", `{"algorithm":"sliding-window","count":60,"key":"client_address","window_seconds":60}`},
		{"/orders/{rest}", "post", "security", `[{"apiward":[]}]`},
		{"/orders/{rest}", "post", "x-apiward-roles", `["CUSTOMER","STAFF"]`},
		{"/orders/{rest}", "post", "x-apiward-rate", `{"algorithm":"sliding-window","count":10,"key":"subject","window_seconds":60}`},
		{"/orders/{rest}", "post", "x-apiward-max-bytes", `8192`},
		{"/orders/{rest}", "post", "x-apiward-target", `"/orders/*"`},
		{"/orders/{rest}", "post", "x-kong-plugin-cors", `{"config":{"origins":["https://shop.example"]},"name":"cors"}`},
		{"/catalog", "get", "security", `[]`},
		{"/staff", "get", "security", `[{"apiward":[]}]`},
		{"/staff", "get", "x-apiward-roles", `["STAFF"]`},
		{"/burst", "get", "x-apiward-rate", `{"algorithm":"sliding-window","count":2,"key":"client_address","window_seconds":1}`},
		{"/hosted", "get", policy.WhenExtension, `{"headers":[{"name":"x-version","value":"2"}],"hosts":["api.example.com","*.example.com"]}`},
	}
	for _, c := range checks {
		if got := encode(t, op(c.path, c.method)[c.member]); got != c.want {
			t.Errorf("%s %s %s = %s, want %s", c.method, c.path, c.member, got, c.want)
		}
	}
	for _, refused := range []string{"/legacy", "/search", "/versions", "/keyed", "/denied", "/monthly", "/odd"} {
		if paths[refused] != nil {
			t.Errorf("%s written although its route is refused", refused)
		}
	}
	if op("/items", "get")["requestBody"] != nil || op("/orders/{rest}", "post")["requestBody"] == nil {
		t.Error("body rules only on body methods")
	}
}

// TestKongTrust checks the RS256 issuer key, the credentials counted and
// the secrets removed from carried plugins.
func TestKongTrust(t *testing.T) {
	ledger := read(t, "shop.yaml").Ledger
	issuers := ledger.Trust.Issuers
	if len(issuers) != 1 || issuers[0].Issuer != "https://login.example" || issuers[0].Audience != nil ||
		!slices.Equal(issuers[0].PublicKeyIDs, []string{"consumer:shop-frontend:0"}) || len(issuers[0].PublicKeys) != 1 {
		t.Fatalf("trust = %+v", issuers)
	}
	want := []exchange.Skipped{{Type: "basicauth_credentials", Count: 1}, {Type: "certificates", Count: 1},
		{Type: "jwt_secrets", Count: 2}, {Type: "keyauth_credentials", Count: 1}, {Type: "plugin_secret_fields", Count: 2}}
	if !slices.Equal(ledger.CredentialsSkipped, want) {
		t.Errorf("credentials_skipped = %v, want %v", ledger.CredentialsSkipped, want)
	}
}

// TestKongRefusesFile checks the whole-file refusals.
func TestKongRefusesFile(t *testing.T) {
	for name, root := range map[string]map[string]any{
		"version":  {"_format_version": "1.1"},
		"missing":  {},
		"defaults": {"_format_version": "3.0", "_info": map[string]any{"defaults": map[string]any{}}},
	} {
		if _, err := Read(root, nil); err == nil {
			t.Errorf("%s: read", name)
		}
	}
}

// TestLedgerComplete checks that each setting of the shop fixture, listed
// here by hand, has an item, and that no item names another source.
func TestLedgerComplete(t *testing.T) {
	settings := []string{"#/services/0", "#/services/0/connect_timeout", "#/services/0/plugins/0",
		"#/consumers/0", "#/consumers/1", "#/consumers/1/plugins/0", "#/plugins/0", "#/certificates/0", "#/upstreams/0"}
	routePlugins := map[int]int{1: 5, 2: 1, 3: 2, 4: 1, 9: 1, 10: 2, 11: 1, 13: 1}
	for route := 0; route <= 13; route++ {
		pointer := "#/services/0/routes/" + strconv.Itoa(route)
		settings = append(settings, pointer)
		for p := 0; p < routePlugins[route]; p++ {
			settings = append(settings, pointer+"/plugins/"+strconv.Itoa(p))
		}
	}
	var sources []string
	for _, item := range read(t, "shop.yaml").Ledger.Items {
		if !slices.Contains(sources, item.Source) {
			sources = append(sources, item.Source)
		}
	}
	slices.Sort(settings)
	slices.Sort(sources)
	if !slices.Equal(settings, sources) {
		t.Errorf("settings %d, item sources %d:\nsettings %v\nsources  %v", len(settings), len(sources), settings, sources)
	}
}

// encode is a value's compact JSON text.
func encode(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
