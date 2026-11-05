// Proxy setup, and the bindings the proxy refuses to forward.

package forward

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// bounds are the dial, response-header and read-idle limits used here.
var bounds = UpstreamTimeouts{Dial: 2 * time.Second, ResponseHeader: 5 * time.Second, ReadIdle: 5 * time.Second}

// The proxy needs a bound for every phase of an exchange, since a zero would
// leave that phase unbounded, and a logger.
func TestNativeForwardingProxyRefusesIncompleteSetup(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	for name, timeouts := range map[string]UpstreamTimeouts{
		"no dial bound":            {ResponseHeader: time.Second, ReadIdle: time.Second},
		"no response-header bound": {Dial: time.Second, ReadIdle: time.Second},
		"no read-idle bound":       {Dial: time.Second, ResponseHeader: time.Second},
		"negative read-idle bound": {Dial: time.Second, ResponseHeader: time.Second, ReadIdle: -time.Second},
	} {
		if p, err := NewUpstreamProxy(timeouts, logger); p != nil || err == nil {
			t.Errorf("%s: %v, %v", name, p, err)
		}
	}
	if p, err := NewUpstreamProxy(bounds, nil); p != nil || err == nil {
		t.Errorf("without a logger: %v, %v", p, err)
	}
	p, err := NewUpstreamProxy(bounds, logger)
	if err != nil {
		t.Fatal(err)
	}
	if tr := p.transport; tr.Proxy != nil || !tr.DisableKeepAlives || !tr.DisableCompression || tr.ResponseHeaderTimeout != bounds.ResponseHeader ||
		!tr.Protocols.HTTP1() || tr.Protocols.HTTP2() || tr.Protocols.UnencryptedHTTP2() {
		t.Fatalf("transport %+v", tr)
	}
}

// A request whose binding is missing, forged or not a literal loopback
// address is sent nowhere, whatever else it carries: the loopback upstream
// it names receives nothing.
func TestUnusableBindingSendsNothing(t *testing.T) {
	var received atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { received.Add(1) }))
	t.Cleanup(upstream.Close)
	addr := upstream.Listener.Addr().String()
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewUpstreamProxy(bounds, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	type otherKey struct{}
	unusable := []struct {
		name  string
		value func(context.Context) context.Context
	}{
		{"no binding", func(ctx context.Context) context.Context { return ctx }},
		{"binding under another key", func(ctx context.Context) context.Context {
			return context.WithValue(ctx, otherKey{}, binding{dial: addr})
		}},
		{"destination as a plain value", func(ctx context.Context) context.Context { return context.WithValue(ctx, bindingKey{}, addr) }},
		{"empty binding", func(ctx context.Context) context.Context { return context.WithValue(ctx, bindingKey{}, binding{}) }},
		{"host name", func(ctx context.Context) context.Context {
			return context.WithValue(ctx, bindingKey{}, binding{dial: "localhost:" + port})
		}},
		{"address not on loopback", func(ctx context.Context) context.Context {
			return context.WithValue(ctx, bindingKey{}, binding{dial: "192.0.2.1:80"})
		}},
		{"no port", func(ctx context.Context) context.Context {
			return context.WithValue(ctx, bindingKey{}, binding{dial: "127.0.0.1"})
		}},
	}
	for _, tc := range unusable {
		r := httptest.NewRequest(http.MethodGet, "/a/x", nil)
		r.Header.Set("X-Gateway-Dial", addr)
		rec := httptest.NewRecorder()
		err := p.Forward(rec, r.WithContext(tc.value(r.Context())))
		status, code := ForwardFailure(err)
		if status != http.StatusServiceUnavailable || code != "gateway_unavailable" || FailureReason(err) != "not_sent" || rec.Body.Len() != 0 || len(rec.Header()) != 0 {
			t.Errorf("%s: %v maps to %d %s (%s), response %d %v", tc.name, err, status, code, FailureReason(err), rec.Code, rec.Header())
		}
	}
	if n := received.Load(); n != 0 {
		t.Fatalf("the upstream received %d requests", n)
	}
}
