// Forwarded path rewrite: the compiler's rules and a real upstream's receipt.

package gateway

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/compile"
	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

// A forward rule is fixed canonical prefixes, and the strip prefix lies
// under every target of its API at a segment boundary; the destination is
// the inbound path rewritten and checked canonical again.
func TestRewriteCompile(t *testing.T) {
	for _, tc := range []struct {
		forward policy.Forward
		field   string
	}{
		{policy.Forward{}, "apis[0].forward"},
		{policy.Forward{StripPrefix: "/"}, "apis[0].forward.strip_prefix"},
		{policy.Forward{StripPrefix: "/orders/"}, "apis[0].forward.strip_prefix"},
		{policy.Forward{AddPrefix: "/v1/*"}, "apis[0].forward.add_prefix"},
		{policy.Forward{AddPrefix: "/v1/../admin"}, "apis[0].forward.add_prefix"},
		{policy.Forward{AddPrefix: "/v%31"}, "apis[0].forward.add_prefix"},
		{policy.Forward{StripPrefix: "/ord"}, "operations[0].target"},
	} {
		p := threeAPIPolicy()
		p.APIs[0].Forward = &tc.forward
		err := compileErr(p)
		if invalid, ok := errors.AsType[*policy.PolicyError](err); !ok || invalid.Code != "REWRITE_INVALID" || invalid.Field != tc.field {
			t.Errorf("%+v: %v, want REWRITE_INVALID at %s", tc.forward, err, tc.field)
		}
	}

	p := threeAPIPolicy()
	p.APIs[0].Forward = &policy.Forward{StripPrefix: "/orders", AddPrefix: "/api/v2"}
	g, err := compile.Compile(p, 1, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	for in, want := range map[string]string{"/orders": "/api/v2", "/orders/7": "/api/v2/7", "/orders/7/": "/api/v2/7/", "/ordersx": ""} {
		_, rule := g.Match("/orders/7", http.MethodGet, compile.Inbound{})
		d, ok := rule.Destination(in)
		if ok != (want != "") || d.Path() != want {
			t.Errorf("%s forwarded as %q (%v), want %q", in, d.Path(), ok, want)
		}
	}
}

// The upstream receives the rewritten path with the query unchanged; the
// admission record keeps the inbound path beside it, and the rewritten path
// itself matches nothing, since every decision is made on the inbound path.
func TestRewriteForward(t *testing.T) {
	e := newEnv(t)
	p := e.policy(e.a, false)
	p.APIs[0].Forward = &policy.Forward{StripPrefix: "/a", AddPrefix: "/v1"}
	e.serveNative(p, forwardTimeouts)

	resp, _ := e.exchange(http.MethodGet, "/a/x?q=1", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rewritten request: %d", resp.StatusCode)
	}
	got := e.a.receipts()
	if len(got) != 1 || got[0].Target != "/v1/x?q=1" {
		t.Fatalf("upstream received %+v", got)
	}
	admitted := e.eventOf(got[0].RequestID, 2)
	if admitted["path"] != "/a/x" || admitted["forwarded_path"] != "/v1/x" || admitted["operation"] != "read-a" {
		t.Fatalf("outcome event %v", admitted)
	}

	if resp, _ := e.exchange(http.MethodGet, "/v1/x", "", ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("the rewritten path was routed: %d", resp.StatusCode)
	}
	e.assertReceipts(1, 0, 0)
}
