// Browser access, response headers and address lists: the compiler's
// refusals, and real requests through the data plane and the Admin API.

package gateway

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

func TestBrowserCompile(t *testing.T) {
	shop := func(p *policy.Policy) *policy.CORS {
		p.APIs[0].CORS = &policy.CORS{Origins: []string{"https://shop.example"}, Methods: []string{"GET"}}
		return p.APIs[0].CORS
	}
	for name, tc := range map[string]struct {
		change func(p *policy.Policy)
		code   string
	}{
		"any origin on a signed-in API": {func(p *policy.Policy) { shop(p).Origins = []string{"*"} }, "CORS_WILDCARD_NOT_ALLOWED"},
		"any origin with credentials": {func(p *policy.Policy) {
			p.APIs[1].CORS = &policy.CORS{Origins: []string{"*"}, Methods: []string{"GET"}, Credentials: true}
			p.Operations[4].Auth = "none"
		}, "CORS_WILDCARD_NOT_ALLOWED"},
		"method no endpoint has":   {func(p *policy.Policy) { shop(p).Methods = []string{"DELETE"} }, "INVALID_VALUE"},
		"origin with a path":       {func(p *policy.Policy) { shop(p).Origins = []string{"https://shop.example/app"} }, "INVALID_VALUE"},
		"origin with default port": {func(p *policy.Policy) { shop(p).Origins = []string{"https://shop.example:443"} }, "INVALID_VALUE"},
		"unknown frame value": {func(p *policy.Policy) {
			p.APIs[0].ResponseHeaders = &policy.ResponseHeaders{Frame: "allow"}
		}, "INVALID_VALUE"},
		"removing the request reference": {func(p *policy.Policy) {
			p.APIs[0].ResponseHeaders = &policy.ResponseHeaders{Remove: []string{"x-request-id"}}
		}, "INVALID_VALUE"},
		"range with host bits": {func(p *policy.Policy) { p.APIs[0].AllowAddresses = []string{"10.1.2.3/8"} }, "INVALID_VALUE"},
		"IPv4-mapped range":    {func(p *policy.Policy) { p.APIs[0].AllowAddresses = []string{"::ffff:10.0.0.0/104"} }, "INVALID_VALUE"},
		"empty allow list":     {func(p *policy.Policy) { p.APIs[0].AllowAddresses = []string{} }, "INVALID_VALUE"},
	} {
		p := threeAPIPolicy()
		tc.change(&p)
		err := compileErr(p)
		if invalid, ok := errors.AsType[*policy.PolicyError](err); !ok || invalid.Code != tc.code {
			t.Errorf("%s: %v, want %s", name, err, tc.code)
		}
	}
	p := threeAPIPolicy()
	shop(&p).Methods = []string{"GET", "POST"}
	p.APIs[0].AllowAddresses = []string{"10.0.0.0/8", "2001:db8::/32"}
	if err := compileErr(p); err != nil {
		t.Fatalf("valid settings: %v", err)
	}
}

// browserPolicy gives API a browser access from one origin, response
// headers and a public rate rule that also limits its preflights.
func (e *env) browserPolicy() policy.Policy {
	p := e.policy(e.a, false)
	maxAge := int64(60)
	p.APIs[0].CORS = &policy.CORS{Origins: []string{"https://shop.example"}, Methods: []string{"GET", "POST"},
		Headers: []string{"Content-Type"}, Expose: []string{"X-Total"}, Credentials: true, MaxAgeSeconds: &maxAge}
	p.APIs[0].ResponseHeaders = &policy.ResponseHeaders{HSTS: true, Nosniff: true, Frame: "deny", Referrer: "no-referrer", Remove: []string{"Server"}}
	p.Operations[0].Rate = &policy.RateRule{Algorithm: "sliding-window", Count: 4, WindowSeconds: 60, Key: "client_address"}
	return p
}

func TestCORS(t *testing.T) {
	e := newEnv(t)
	e.a.handle = func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Access-Control-Allow-Origin", "https://evil.example")
		w.Header().Set("Server", "upstream/1.0")
		w.Header().Set("Referrer-Policy", "same-origin")
		return false
	}
	e.serveNative(e.browserPolicy(), forwardTimeouts)
	origin := []string{"Origin", "https://shop.example"}
	preflight := func(extra ...string) http.Header {
		resp, _ := e.exchange(http.MethodOptions, "/a/x", "", "", append(slices.Clone(extra), "Access-Control-Request-Method", "GET")...)
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("preflight %v: %d", extra, resp.StatusCode)
		}
		return resp.Header
	}
	allowed := preflight(append(origin, "Access-Control-Request-Headers", "content-type")...)
	if allowed.Get("Access-Control-Allow-Origin") != "https://shop.example" || allowed.Get("Access-Control-Allow-Methods") != "GET, POST" ||
		allowed.Get("Access-Control-Allow-Headers") != "Content-Type" || allowed.Get("Access-Control-Max-Age") != "60" ||
		allowed.Get("Access-Control-Allow-Credentials") != "true" || allowed.Get("Vary") != "Origin" {
		t.Fatalf("allowed preflight headers %v", allowed)
	}
	for _, refused := range [][]string{
		{"Origin", "https://evil.example"},
		append(slices.Clone(origin), "Access-Control-Request-Headers", "X-Secret"),
	} {
		if h := preflight(refused...); h.Get("Access-Control-Allow-Origin") != "" || h.Get("Access-Control-Allow-Methods") != "" {
			t.Errorf("refused preflight %v answered %v", refused, h)
		}
	}
	// Preflights have their own counter under the API's public rate rule:
	// the fifth from one address is refused.
	preflight(origin...)
	if resp, _ := e.exchange(http.MethodOptions, "/a/x", "", "", append(slices.Clone(origin), "Access-Control-Request-Method", "GET")...); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("fifth preflight: %d", resp.StatusCode)
	}
	if e.a.count() != 0 {
		t.Fatalf("a preflight reached the upstream")
	}

	// The upstream's CORS headers are replaced, listed headers removed and
	// missing security headers added; HSTS needs TLS.
	resp, _ := e.exchange(http.MethodGet, "/a/x", "", "", origin...)
	h := resp.Header
	if resp.StatusCode != http.StatusOK || h.Get("Access-Control-Allow-Origin") != "https://shop.example" || h.Get("Access-Control-Expose-Headers") != "X-Total" ||
		h.Get("Server") != "" || h.Get("X-Frame-Options") != "DENY" || h.Get("X-Content-Type-Options") != "nosniff" ||
		h.Get("Referrer-Policy") != "same-origin" || h.Get("Strict-Transport-Security") != "" {
		t.Fatalf("actual response %d %v", resp.StatusCode, h)
	}
	// A refusal carries them too; another origin gets no CORS header.
	refusal, _ := e.exchange(http.MethodPost, "/a/item", "application/json", "[]", origin...)
	if refusal.StatusCode != http.StatusBadRequest || refusal.Header.Get("Access-Control-Allow-Origin") != "https://shop.example" {
		t.Fatalf("refusal %d %v", refusal.StatusCode, refusal.Header)
	}
	if other, _ := e.exchange(http.MethodGet, "/a/y", "", "", "Origin", "https://evil.example"); other.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("other origin answered %v", other.Header)
	}
	events := e.eventsWhen(func(events []map[string]any) bool { return countKind(events, "request.preflight") >= 4 })
	codes := map[string]int{}
	for _, ev := range events {
		if ev["kind"] == "request.preflight" && ev["stage"] == "ROUTE" && ev["status"] == float64(http.StatusNoContent) {
			codes[ev["decision"].(string)+"/"+ev["error"].(string)]++
		}
	}
	if codes["allowed/cors_preflight"] != 2 || codes["refused/cors_refused"] != 2 {
		t.Fatalf("preflight events %v", codes)
	}
}

func TestAddressLists(t *testing.T) {
	e := newEnv(t)
	p := e.policy(e.a, true)
	p.APIs[0].AllowAddresses = []string{"10.0.0.0/8"}
	p.APIs[1].AllowAddresses = []string{"127.0.0.0/8", "::1/128"}
	c, _ := e.serveNative(p, forwardTimeouts)
	if status, body := e.get("/a/x"); status != http.StatusForbidden || !strings.Contains(body, "address_not_allowed") {
		t.Fatalf("address outside the allow list: %d %s", status, body)
	}
	if status, _ := e.get("/b/x"); status != http.StatusOK {
		t.Fatalf("address inside the allow list: %d", status)
	}

	block := func(method, cidr, body, key string) (int, string) {
		code, data, _ := e.harnessCall(method, "/admin/blocked-addresses/"+strings.ReplaceAll(cidr, "/", "%2F"), body, "Idempotency-Key", key)
		return code, data
	}
	for _, bad := range []struct{ cidr, body string }{
		{"127.0.0.1/8", `{"reason":"scan"}`},
		{"127.0.0.1/32", `{"reason":"scan","expires_at":"2020-01-01T00:00:00Z"}`},
	} {
		if code, _ := block(http.MethodPut, bad.cidr, bad.body, "block-address-bad-"+strings.ReplaceAll(bad.cidr, "/", "-")); code != http.StatusBadRequest {
			t.Errorf("block %s %s: %d", bad.cidr, bad.body, code)
		}
	}
	// An IPv4-mapped single address is stored as its IPv4 range.
	if code, data := block(http.MethodPut, "::ffff:127.0.0.1/128", `{"reason":"scan"}`, "block-address-key-0001"); code != http.StatusOK || !strings.Contains(data, `"range":"127.0.0.1/32"`) {
		t.Fatalf("block: %d %s", code, data)
	}
	if status, body := e.get("/b/x"); status != http.StatusForbidden || !strings.Contains(body, "address_blocked") {
		t.Fatalf("blocked address: %d %s", status, body)
	}
	if code, data, _ := e.harnessCall(http.MethodGet, "/admin/blocked-addresses", ""); code != http.StatusOK || !strings.Contains(data, `"expires_at":null`) {
		t.Fatalf("list: %d %s", code, data)
	}
	if code, _ := block(http.MethodDelete, "127.0.0.1/32", "", "unblock-address-key-0001"); code != http.StatusOK {
		t.Fatalf("unblock: %d", code)
	}
	if status, _ := e.get("/b/x"); status != http.StatusOK {
		t.Fatalf("after unblocking: %d", status)
	}
	events := e.eventsWhen(func(events []map[string]any) bool { return hasKinds(events, "address.block", "address.unblock") })
	if !hasKinds(events, "address.block", "address.unblock") {
		t.Fatal("block and unblock were not recorded")
	}
	// An entry stays in the store; one with an expiry ends by itself.
	if code, data := block(http.MethodPut, "203.0.113.0/24", `{"reason":"scan","expires_at":"`+time.Now().Add(time.Hour).UTC().Format(time.RFC3339)+`"}`, "block-address-key-0002"); code != http.StatusOK {
		t.Fatalf("block with expiry: %d %s", code, data)
	}
	if err := c.Stop(); err != nil {
		t.Fatal(err)
	}
	if n := queryInt(t, e.database, `SELECT COUNT(*) FROM blocked_address WHERE cidr = '203.0.113.0/24' AND blocked_by = '`+harnessAdmin+`' AND expires_at IS NOT NULL`); n != 1 {
		t.Fatalf("%d stored entries", n)
	}
	if n := queryInt(t, e.database, `SELECT COUNT(*) FROM blocked_address`); n != 1 {
		t.Fatalf("%d entries after unblocking one of two", n)
	}
}
