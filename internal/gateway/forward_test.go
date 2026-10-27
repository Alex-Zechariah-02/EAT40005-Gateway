package gateway

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// These tests serve the enforcement handler with the native reverse proxy as
// its forwarder, under a plain net/http server in the test process, and
// forward to real loopback upstreams: the deployment's receivers, the three
// fixture processes, or a receiver whose handler stages a failure. What an
// upstream received, or that it received nothing, is read from the upstream
// itself.

// forwardTimeouts are the legacy dial and response-header bounds and the
// read-idle bound of the native transport.
var forwardTimeouts = upstreamTimeouts{dial: 2 * time.Second, responseHeader: 5 * time.Second, readIdle: 5 * time.Second}

// serveNative seeds e's database with policy, owns and starts its controller,
// and serves the enforcement handler with the native proxy, bounded by
// timeouts, at e.dataAddr, where the harness request helpers send. It returns
// the controller and the proxy's log.
func (e *env) serveNative(policy Policy, timeouts upstreamTimeouts) (*Controller, *logBuffer) {
	e.t.Helper()
	seed(e.t, e.database, policy, seedOptions{})
	c, _ := e.own(e.controllerConfig())
	if err := c.Start(); err != nil {
		e.t.Fatal(err)
	}
	logs := &logBuffer{}
	p, err := newUpstreamProxy(timeouts, slog.New(slog.NewJSONHandler(logs, nil)))
	if err != nil {
		e.t.Fatal(err)
	}
	h, err := newHandler(c, 0, p.forward)
	if err != nil {
		e.t.Fatal(err)
	}
	// As in production, "OPTIONS *" reaches enforcement instead of net/http.
	server := httptest.NewUnstartedServer(h)
	server.Config.DisableGeneralOptionsHandler = true
	server.Start()
	e.t.Cleanup(server.Close)
	e.dataAddr = server.Listener.Addr().String()
	return c, logs
}

// openResponse sends request byte for byte and reads the response head,
// leaving the body to the caller. The connection closes when the test ends.
func (e *env) openResponse(request string) (*http.Response, net.Conn) {
	e.t.Helper()
	conn, err := net.DialTimeout("tcp", e.dataAddr, 5*time.Second)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	if _, err := io.WriteString(conn, request); err != nil {
		e.t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		e.t.Fatalf("reading the response to %q: %v", request, err)
	}
	return resp, conn
}

// fetch sends one GET from a goroutine other than the test's own, reporting
// failures instead of ending the test.
func fetch(addr, path string) (int, string, error) {
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 30 * time.Second}
	resp, err := client.Get("http://" + addr + path)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data), err
}

// outcomeOf returns the outcome event of an admitted request.
func (e *env) outcomeOf(requestID string) map[string]any {
	e.t.Helper()
	return e.eventOf(requestID, 2)
}

// failedOutcome reports whether ev records a forwarding failure answered
// with status and code, with the given delivery reason.
func failedOutcome(ev map[string]any, status int, code, reason string) bool {
	got, _ := ev["reason"].(string)
	return ev["kind"] == "request.forwarded" && ev["decision"] == "upstream_failure" &&
		ev["status"] == float64(status) && ev["error"] == code && got == reason
}

// The proxy needs a bound for every phase of an exchange, since a zero would
// leave that phase unbounded, and a logger.
func TestNativeForwardingProxyRefusesIncompleteSetup(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	for name, timeouts := range map[string]upstreamTimeouts{
		"no dial bound":            {responseHeader: time.Second, readIdle: time.Second},
		"no response-header bound": {dial: time.Second, readIdle: time.Second},
		"no read-idle bound":       {dial: time.Second, responseHeader: time.Second},
		"negative read-idle bound": {dial: time.Second, responseHeader: time.Second, readIdle: -time.Second},
	} {
		if p, err := newUpstreamProxy(timeouts, logger); p != nil || err == nil {
			t.Errorf("%s: %v, %v", name, p, err)
		}
	}
	if p, err := newUpstreamProxy(forwardTimeouts, nil); p != nil || err == nil {
		t.Errorf("without a logger: %v, %v", p, err)
	}
	p, err := newUpstreamProxy(forwardTimeouts, logger)
	if err != nil {
		t.Fatal(err)
	}
	if tr := p.transport; tr.Proxy != nil || !tr.DisableKeepAlives || !tr.DisableCompression || tr.ResponseHeaderTimeout != forwardTimeouts.responseHeader ||
		!tr.Protocols.HTTP1() || tr.Protocols.HTTP2() || tr.Protocols.UnencryptedHTTP2() {
		t.Fatalf("transport %+v", tr)
	}
}

// Every catalogue step runs through the native proxy against the three
// fixture processes: each admitted request reaches exactly its fixture, which
// runs the intended operation with the expected outcome and state change and
// sees the gateway's identity headers, and every step the gateway refuses
// reaches no fixture.
func TestNativeForwardingThroughFixtures(t *testing.T) {
	fixtures := startFixtures(t)
	e := newEnv(t)
	e.serveNative(fixturePolicy(fixtures), forwardTimeouts)
	c := loadCatalogue(t)

	results := runScenarios(t, e, fixtures, c)
	if len(results) != len(c.Scenarios) {
		t.Fatalf("%d of %d steps ran", len(results), len(c.Scenarios))
	}
	ran := map[string]bool{}
	for _, r := range results {
		ran[r.Operation] = true
	}
	for _, op := range c.Operations {
		if !ran[op.ID] {
			t.Errorf("catalogue operation %s was never exercised", op.ID)
		}
	}
	e.assertReceipts(0, 0, 0)
}

// The destination is the binding the handler made from the captured
// generation and nothing else. No client field moves a request; the inbound
// Host reaches the upstream but chooses nothing. A request whose binding is
// missing, forged or not a literal loopback address is sent nowhere: no
// fallback destination exists.
func TestNativeForwardingBinding(t *testing.T) {
	e := newEnv(t)
	e.serveNative(e.policy(e.a, true), forwardTimeouts)

	// Every field a client could use to name a destination names C.
	forged := "Host: " + e.c.addr + "\r\nX-Forwarded-Host: " + e.c.addr + "\r\nForwarded: host=" + e.c.addr +
		"\r\nX-Gateway-Upstream: C\r\nX-Gateway-Dial: " + e.c.addr + "\r\nConnection: close\r\n\r\n"
	for _, target := range []string{"/a/x", "/a/x?upstream=C&dial=" + e.c.addr, "/a/" + e.c.addr} {
		if code, body := e.raw("GET " + target + " HTTP/1.1\r\n" + forged); code != http.StatusOK || body != "A ok" {
			t.Errorf("%s with forged destination fields: %d %q", target, code, body)
		}
	}
	if code, body := e.raw("GET /b/x HTTP/1.1\r\nHost: " + e.a.addr + "\r\nConnection: close\r\n\r\n"); code != http.StatusOK || body != "B ok" {
		t.Errorf("/b/x with a Host naming A: %d %q", code, body)
	}
	if code, _ := e.raw("GET http://" + e.c.addr + "/a/x HTTP/1.1\r\nHost: " + e.c.addr + "\r\nConnection: close\r\n\r\n"); code != http.StatusBadRequest {
		t.Errorf("absolute-form target naming C: %d", code)
	}
	e.assertReceipts(3, 1, 0)
	for _, r := range e.a.receipts() {
		if r.Host != e.c.addr || r.Headers.Get("X-Gateway-Upstream") != "" || r.Headers.Get("X-Gateway-Dial") != "" ||
			r.Headers.Get("Forwarded") != "" || r.Headers.Get("X-Forwarded-Host") != e.c.addr {
			t.Errorf("A received Host %q and headers %v", r.Host, r.Headers)
		}
	}

	// The proxy sends nothing without a usable binding, whatever else the
	// request carries.
	p, err := newUpstreamProxy(forwardTimeouts, slog.New(slog.DiscardHandler))
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
			return context.WithValue(ctx, otherKey{}, binding{dial: e.c.addr})
		}},
		{"destination as a plain value", func(ctx context.Context) context.Context { return context.WithValue(ctx, bindingKey{}, e.c.addr) }},
		{"empty binding", func(ctx context.Context) context.Context { return context.WithValue(ctx, bindingKey{}, binding{}) }},
		{"host name", func(ctx context.Context) context.Context {
			return context.WithValue(ctx, bindingKey{}, binding{dial: "localhost:" + port(e.c.addr)})
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
		r.Header.Set("X-Gateway-Dial", e.c.addr)
		rec := httptest.NewRecorder()
		err := p.forward(rec, r.WithContext(tc.value(r.Context())))
		status, code := forwardFailure(err)
		if status != http.StatusServiceUnavailable || code != "gateway_unavailable" || failureReason(err) != "not_sent" || rec.Body.Len() != 0 || len(rec.Header()) != 0 {
			t.Errorf("%s: %v maps to %d %s (%s), response %d %v", tc.name, err, status, code, failureReason(err), rec.Code, rec.Header())
		}
	}
	e.assertReceipts(3, 1, 0)
}

// A request keeps the destination of the generation it captured while
// another generation is published during its exchange; the next request
// uses the new one.
func TestNativeForwardingKeepsCapturedDestination(t *testing.T) {
	e := newEnv(t)
	c, _ := e.serveNative(e.policy(e.a, false), forwardTimeouts)
	captured := c.Current().ID
	entered, release := make(chan struct{}), make(chan struct{})
	e.a.handle = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/a/slow" {
			close(entered)
			<-release
		}
		return false
	}
	type result struct {
		code int
		body string
		err  error
	}
	slow := make(chan result, 1)
	go func() {
		code, body, err := fetch(e.dataAddr, "/a/slow")
		slow <- result{code, body, err}
	}()
	<-entered
	outcome, status := c.Publish(context.Background(), "test", Change{ID: "move-a", Expected: 1, Policy: mustJSON(e.policy(e.b, false))})
	close(release)
	if status != http.StatusOK {
		t.Fatalf("publication during the exchange: %d %+v", status, outcome)
	}
	if r := <-slow; r.err != nil || r.code != http.StatusOK || r.body != "A ok" {
		t.Fatalf("request in progress during the publication: %d %q %v", r.code, r.body, r.err)
	}
	if code, body := e.get("/a/x"); code != http.StatusOK || body != "B ok" {
		t.Fatalf("request after the publication: %d %q", code, body)
	}
	e.assertReceipts(1, 1, 0)
	if a, b := e.a.receipts()[0], e.b.receipts()[0]; a.Generation != captured || b.Generation != outcome.Generation {
		t.Fatalf("A received generation %s (want %s), B %s (want %s)", a.Generation, captured, b.Generation, outcome.Generation)
	}
}

// Concurrent allowed and denied requests while publications move API a
// between A and B: every admitted request reaches the upstream of the
// generation it captured, its outcome event names that generation, and no
// denied request reaches any upstream.
func TestNativeForwardingUnderConcurrency(t *testing.T) {
	e := newEnv(t)
	toA, toB := e.policy(e.a, false), e.policy(e.b, false)
	c, _ := e.serveNative(toA, forwardTimeouts)
	receiverOf := map[string]string{c.Current().ID: "A"}
	var mu sync.Mutex

	done := make(chan struct{})
	var publisher sync.WaitGroup
	publisher.Go(func() {
		expected := int64(1)
		for i := 0; ; i++ {
			select {
			case <-done:
				return
			default:
			}
			policy, name := toB, "B"
			if i%2 == 1 {
				policy, name = toA, "A"
			}
			outcome, status := c.Publish(context.Background(), "test", Change{ID: fmt.Sprintf("concurrent-%d", i), Expected: expected, Policy: mustJSON(policy)})
			if status != http.StatusOK {
				t.Errorf("publication %d: %d %+v", i, status, outcome)
				return
			}
			mu.Lock()
			receiverOf[outcome.Generation] = name
			mu.Unlock()
			expected = outcome.Revision
		}
	})
	var clients sync.WaitGroup
	for range 8 {
		clients.Go(func() {
			for range 25 {
				if code, body, err := fetch(e.dataAddr, "/a/x"); err != nil || code != http.StatusOK || (body != "A ok" && body != "B ok") {
					t.Errorf("allowed request: %d %q %v", code, body, err)
				}
			}
		})
	}
	for range 4 {
		clients.Go(func() {
			for range 10 {
				if code, _, err := fetch(e.dataAddr, "/d"); err != nil || code != http.StatusForbidden {
					t.Errorf("denied request: %d %v", code, err)
				}
			}
		})
	}
	clients.Wait()
	close(done)
	publisher.Wait()

	receipts := map[string][]receipt{"A": e.a.receipts(), "B": e.b.receipts()}
	if total := len(receipts["A"]) + len(receipts["B"]); total != 200 || e.c.count() != 0 {
		t.Fatalf("receipts A=%d B=%d C=%d, want 200 in all at A and B", len(receipts["A"]), len(receipts["B"]), e.c.count())
	}
	outcomes := map[string]string{}
	for _, ev := range e.eventsWhen(func(events []map[string]any) bool { return countKind(events, "request.forwarded") == 200 }) {
		if ev["kind"] == "request.forwarded" {
			outcomes[ev["request_id"].(string)], _ = ev["generation"].(string)
		}
	}
	for name, got := range receipts {
		for _, r := range got {
			if receiverOf[r.Generation] != name || outcomes[r.RequestID] != r.Generation || r.Path != "/a/x" {
				t.Fatalf("%s received a request under %s (whose upstream is %q), outcome under %s", name, r.Generation, receiverOf[r.Generation], outcomes[r.RequestID])
			}
		}
	}
	t.Logf("%d generations, receipts A=%d B=%d", len(receiverOf), len(receipts["A"]), len(receipts["B"]))
}

// The forwarded target is the admitted one: canonical path matching decides
// the operation and the upstream receives the client's path in its received
// encoding and its query byte for byte. Targets the gateway refuses, and
// tunnels and protocol switches, reach no upstream.
func TestNativeForwardingRoutingTable(t *testing.T) {
	e := newEnv(t)
	policy := Policy{
		SchemaVersion: schemaVersion,
		Profile:       profileCore,
		Upstreams:     []Upstream{{ID: "A", Dial: e.a.addr}, {ID: "B", Dial: e.b.addr}},
		APIs:          []API{{ID: "orders", Upstream: "A"}, {ID: "reports", Upstream: "B"}},
		Operations: []Operation{
			{ID: "reports", API: "reports", Method: "GET", Target: "/reports", Access: "allow", Auth: "none"},
			{ID: "order", API: "orders", Method: "GET", Target: "/orders/*", Access: "allow", Auth: "none"},
			{ID: "create", API: "orders", Method: "POST", Target: "/orders", Access: "allow", Auth: "none", Body: jsonBody(1024, anyObject)},
			{ID: "archive", API: "orders", Method: "DELETE", Target: "/orders/archive", Access: "deny", Auth: "none"},
		},
	}
	e.serveNative(policy, forwardTimeouts)

	rows := []struct {
		request string
		status  int
		to      *receiver
		target  string // raw target at the receiver
		path    string // decoded path at the receiver
	}{
		{"GET /orders/42 HTTP/1.1", 200, e.a, "/orders/42", "/orders/42"},
		{"GET /orders/42/items HTTP/1.1", 200, e.a, "/orders/42/items", "/orders/42/items"},
		{"GET /orders/42?x=1&y=2 HTTP/1.1", 200, e.a, "/orders/42?x=1&y=2", "/orders/42"},
		{"GET /orders/42?b=1;c=2&d=%zz&e=%20f HTTP/1.1", 200, e.a, "/orders/42?b=1;c=2&d=%zz&e=%20f", "/orders/42"},
		{"GET /orders/42? HTTP/1.1", 200, e.a, "/orders/42?", "/orders/42"},
		{"GET /orders/%41 HTTP/1.1", 200, e.a, "/orders/%41", "/orders/A"},
		{"GET /orders/a;b=c HTTP/1.1", 200, e.a, "/orders/a;b=c", "/orders/a;b=c"},
		{"GET /orders/%3Bx HTTP/1.1", 200, e.a, "/orders/%3Bx", "/orders/;x"},
		{"GET /reports HTTP/1.1", 200, e.b, "/reports", "/reports"},
		{"GET /reports HTTP/1.0", 200, e.b, "/reports", "/reports"},
		{"GET /orders HTTP/1.1", 405, nil, "", ""},
		{"GET /orders/ HTTP/1.1", 404, nil, "", ""},
		{"GET /Orders/42 HTTP/1.1", 404, nil, "", ""},
		{"GET /ordersX HTTP/1.1", 404, nil, "", ""},
		{"GET /orders/%2F42 HTTP/1.1", 400, nil, "", ""},
		{"GET /orders/../reports HTTP/1.1", 400, nil, "", ""},
		{"GET /orders/%2e%2e/reports HTTP/1.1", 400, nil, "", ""},
		{"GET /orders//42 HTTP/1.1", 400, nil, "", ""},
		{"GET /orders/%252F HTTP/1.1", 400, nil, "", ""},
		{"GET /orders/a%5Cb HTTP/1.1", 400, nil, "", ""},
		{"get /orders/42 HTTP/1.1", 405, nil, "", ""},
		{"HEAD /orders/42 HTTP/1.1", 405, nil, "", ""},
		{"DELETE /orders/archive HTTP/1.1", 403, nil, "", ""},
		{"GET http://" + e.b.addr + "/reports HTTP/1.1", 400, nil, "", ""},
		{"CONNECT " + e.b.addr + " HTTP/1.1", 400, nil, "", ""},
		{"OPTIONS * HTTP/1.1", 400, nil, "", ""},
	}
	for _, r := range rows {
		e.resetReceivers()
		code, _ := e.raw(r.request + "\r\nHost: gw\r\nConnection: close\r\n\r\n")
		if code != r.status {
			t.Errorf("%s: status %d, want %d", r.request, code, r.status)
			continue
		}
		total := e.a.count() + e.b.count() + e.c.count()
		if r.to == nil {
			if total != 0 {
				t.Errorf("%s: a refused request reached an upstream", r.request)
			}
			continue
		}
		got := r.to.receipts()
		if total != 1 || len(got) != 1 || got[0].Target != r.target || got[0].Path != r.path || got[0].Method != "GET" {
			t.Errorf("%s: receipts %+v (total %d), want one at %s with target %q path %q", r.request, got, total, r.to.name, r.target, r.path)
		}
	}

	// Protocol switches and tunnels are refused at ingress, with an event.
	switches := []string{
		"GET /orders/42 HTTP/1.1\r\nHost: gw\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n",
		"GET /orders/42 HTTP/1.1\r\nHost: gw\r\nUpgrade: websocket\r\n\r\n",
		"GET /orders/42 HTTP/1.1\r\nHost: gw\r\nConnection: Upgrade, HTTP2-Settings\r\nUpgrade: h2c\r\nHTTP2-Settings: AAMAAABkAAQAAP__\r\n\r\n",
	}
	e.resetReceivers()
	for _, request := range switches {
		resp, body := e.rawResponse(request)
		_, id := requestID(resp, body)
		if events := e.requestEvents(id, 1); resp.StatusCode != http.StatusBadRequest || len(events) != 1 || events[0]["stage"] != "INGRESS" {
			t.Errorf("%q: %d %s, events %v", request, resp.StatusCode, body, events)
		}
	}
	e.assertReceipts(0, 0, 0)
}

// An admitted request reaches the upstream with its method, target, body
// bytes and end-to-end headers unchanged and its body length declared,
// without the client credential, the proxy credential or any field the
// gateway did not receive; the upstream response returns unchanged.
func TestNativeForwardingRequestIntact(t *testing.T) {
	e := newEnv(t)
	c, logs := e.serveNative(e.policy(e.a, false), forwardTimeouts)
	g := c.Current().ID
	e.a.handle = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPost {
			return false
		}
		w.Header().Set("X-Upstream", "kept")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Add("Set-Cookie", "a=1")
		w.Header().Add("Set-Cookie", "b=2")
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"ok":true}`)
		return true
	}
	body := "{ \"n\" : 1,\t\"s\":\"é\\u00e9\" }"
	token := "Bearer " + issuer.tokenFor(t, "alice")
	resp, reply := e.rawResponse(fmt.Sprintf("POST /a/item?q=1&q=2&x=%%20y HTTP/1.1\r\nHost: gw\r\nConnection: close\r\n"+
		"Content-Type: application/json; charset=utf-8\r\nContent-Length: %d\r\nX-Custom: kept\r\nAccept: application/json\r\n"+
		"Cookie: c=3\r\nUser-Agent: test-agent/1\r\nIdempotency-Key: key-0001\r\nAuthorization: %s\r\n"+
		"Proxy-Authorization: Basic c2VjcmV0\r\n\r\n%s", len(body), token, body))
	if resp.StatusCode != http.StatusCreated || reply != `{"ok":true}` || resp.Header.Get("X-Upstream") != "kept" ||
		strings.Join(resp.Header.Values("Set-Cookie"), "|") != "a=1|b=2" || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("response %d %v %q", resp.StatusCode, resp.Header, reply)
	}
	got := e.a.receipts()
	if len(got) != 1 {
		t.Fatalf("%d receipts, want 1", len(got))
	}
	r, h := got[0], got[0].Headers
	want := map[string]string{
		"Content-Type": "application/json; charset=utf-8", "Content-Length": fmt.Sprint(len(body)), "X-Custom": "kept",
		"Accept": "application/json", "Cookie": "c=3", "User-Agent": "test-agent/1", "Idempotency-Key": "key-0001",
		"Transfer-Encoding": "", "Accept-Encoding": "", "Proxy-Authorization": "", "Authorization": "",
	}
	for name, value := range want {
		if strings.Join(h.Values(name), "|") != value {
			t.Errorf("upstream received %s %q, want %q", name, h.Values(name), value)
		}
	}
	switch {
	case r.Method != http.MethodPost || r.Target != "/a/item?q=1&q=2&x=%20y" || r.Body != body:
		t.Errorf("request line or body changed: %s %s %q", r.Method, r.Target, r.Body)
	case r.Generation != g || len(r.RequestID) != 32:
		t.Errorf("gateway headers: generation %q request id %q", r.Generation, r.RequestID)
	}

	// Without a User-Agent or Accept-Encoding from the client, the upstream
	// receives none.
	if code, _ := e.raw("GET /a/x HTTP/1.1\r\nHost: gw\r\nConnection: close\r\n\r\n"); code != http.StatusOK {
		t.Fatalf("plain request: %d", code)
	}
	if h := e.a.receipts()[1].Headers; h.Get("User-Agent") != "" || h.Get("Accept-Encoding") != "" {
		t.Errorf("fields the client did not send: User-Agent %q, Accept-Encoding %q", h.Get("User-Agent"), h.Get("Accept-Encoding"))
	}
	for name, text := range map[string]string{"audit log": fmt.Sprint(e.auditEvents()), "proxy log": logs.String()} {
		if strings.Contains(text, token[7:]) || strings.Contains(text, "c2VjcmV0") {
			t.Errorf("the %s holds a credential", name)
		}
	}
	e.assertReceipts(2, 0, 0)
}

// The forwarding metadata as the upstream and the client receive it: the
// Host and the raw query are kept; hop-by-hop fields are removed, except a TE
// that asks for trailers, and the transport closes its connection after the
// exchange; X-Forwarded-For carries the direct peer only, X-Forwarded-Host
// the inbound Host and X-Forwarded-Proto "http", never the client's values;
// the client's Forwarded field is removed; Via names the gateway after any
// earlier hop. Fields the client names in Connection are removed before the
// gateway sets its own, so naming them removes nothing the gateway sets. The
// response keeps the upstream's Server field, loses its hop-by-hop fields,
// including those it names in Connection, and gains Via.
func TestNativeForwardingMetadata(t *testing.T) {
	e := newEnv(t)
	e.serveNative(e.policy(e.a, false), forwardTimeouts)
	// Written raw: net/http would replace the Connection field of a handler's
	// response to a request that asked to close. The field does not say close
	// either: net/http drops the whole Connection field of a response that
	// does, so the proxy could not see a field it names.
	e.a.handle = func(w http.ResponseWriter, r *http.Request) bool {
		conn, buffered, _ := http.NewResponseController(w).Hijack()
		defer conn.Close()
		buffered.WriteString("HTTP/1.1 200 OK\r\nServer: upstream-server\r\nKeep-Alive: timeout=99\r\nConnection: X-Hop\r\n" +
			"X-Hop: upstream-only\r\nProxy-Authenticate: Basic\r\nContent-Length: 4\r\n\r\nA ok")
		buffered.Flush()
		return true
	}
	forged := "X-Forwarded-For: 203.0.113.9\r\nX-Forwarded-Host: evil.example\r\nX-Forwarded-Proto: https\r\n" +
		"Forwarded: for=203.0.113.9;host=evil.example\r\nVia: 1.0 client-proxy\r\n" +
		"Keep-Alive: timeout=5\r\nProxy-Connection: keep-alive\r\nTE: trailers\r\n"
	cases := []struct {
		name, connection, via string
	}{
		{"hop-by-hop fields", "close", "1.0 client-proxy|1.1 gateway"},
		{"forwarding fields named in Connection", "close, X-Forwarded-For, x-forwarded-host,Via, X-Forwarded-Proto, Forwarded", "1.1 gateway"},
	}
	for i, tc := range cases {
		resp, body := e.rawResponse("GET /a/x?b=1;c=2&d=%zz HTTP/1.1\r\nHost: gw.example:8443\r\n" + forged + "Connection: " + tc.connection + "\r\n\r\n")
		if resp.StatusCode != http.StatusOK || body != "A ok" {
			t.Fatalf("%s: response %d %q", tc.name, resp.StatusCode, body)
		}
		got := e.a.receipts()[i]
		h := got.Headers
		checks := []struct {
			name, got, want string
		}{
			{"request-target", got.Target, "/a/x?b=1;c=2&d=%zz"},
			{"Host", got.Host, "gw.example:8443"},
			{"X-Forwarded-For", strings.Join(h.Values("X-Forwarded-For"), "|"), "127.0.0.1"},
			{"X-Forwarded-Host", strings.Join(h.Values("X-Forwarded-Host"), "|"), "gw.example:8443"},
			{"X-Forwarded-Proto", strings.Join(h.Values("X-Forwarded-Proto"), "|"), "http"},
			{"Forwarded", strings.Join(h.Values("Forwarded"), "|"), ""},
			{"Via", strings.Join(h.Values("Via"), "|"), tc.via},
			{"Te", h.Get("Te"), "trailers"},
			{"Keep-Alive", h.Get("Keep-Alive"), ""},
			{"Proxy-Connection", h.Get("Proxy-Connection"), ""},
			{"Connection", h.Get("Connection"), "close"},
			{"response Via", strings.Join(resp.Header.Values("Via"), "|"), "1.1 gateway"},
			{"response Server", resp.Header.Get("Server"), "upstream-server"},
			{"response Keep-Alive", resp.Header.Get("Keep-Alive"), ""},
			{"response X-Hop", resp.Header.Get("X-Hop"), ""},
			{"response Proxy-Authenticate", resp.Header.Get("Proxy-Authenticate"), ""},
		}
		for _, c := range checks {
			if c.got != c.want {
				t.Errorf("%s: %s %q, want %q", tc.name, c.name, c.got, c.want)
			}
		}
	}
	e.assertReceipts(2, 0, 0)
}

// Request bodies on the native path: the effective ceiling, the smaller of
// the global one (16384 bytes) and the operation's, bounds declared and
// chunked bodies alike; a body within it reaches the upstream byte for byte
// with its length declared, and one byte more reaches nothing. A chunked
// body's trailer fields stay behind. Expect: 100-continue behaves as before.
// Conflicting framing cannot carry a second request upstream.
func TestNativeForwardingBodies(t *testing.T) {
	e := newEnv(t)
	p := e.policy(e.a, false)
	p.Operations = append(p.Operations,
		Operation{ID: "large", API: "a", Method: "POST", Target: "/a/large", Access: "allow", Auth: "none", Body: jsonBody(32768, anyObject)},
		Operation{ID: "small", API: "a", Method: "POST", Target: "/a/small", Access: "allow", Auth: "none", Body: jsonBody(8192, anyObject)},
		Operation{ID: "equal", API: "a", Method: "POST", Target: "/a/equal", Access: "allow", Auth: "none", Body: jsonBody(16384, anyObject)},
	)
	e.serveNative(p, forwardTimeouts)
	const global = 16384
	ceilings := []struct {
		path    string
		ceiling int
	}{{"/a/large", global}, {"/a/small", 8192}, {"/a/equal", global}}
	received := 0
	for _, op := range ceilings {
		for _, framing := range []string{"declared", "chunked"} {
			for _, size := range []int{op.ceiling, op.ceiling + 1} {
				name := fmt.Sprintf("%s, %s, %d bytes", op.path, framing, size)
				body := sizedBody("", size)
				status, reply := e.sendBody(framing, op.path, body)
				if size > op.ceiling {
					if status != http.StatusRequestEntityTooLarge || e.a.count() != received {
						t.Errorf("%s: %d (%s) with %d receipts, want 413 and none new", name, status, reply, e.a.count())
					}
					continue
				}
				received++
				got := e.a.receipts()
				if status != http.StatusOK || len(got) != received {
					t.Fatalf("%s: %d (%s) with %d receipts, want 200 and %d", name, status, reply, len(got), received)
				}
				last := got[received-1]
				if last.Body != body || last.Headers.Get("Content-Length") != fmt.Sprint(size) || last.Headers.Get("Transfer-Encoding") != "" {
					t.Errorf("%s: upstream received %d bytes (intact %v), Content-Length %q, Transfer-Encoding %q",
						name, len(last.Body), last.Body == body, last.Headers.Get("Content-Length"), last.Headers.Get("Transfer-Encoding"))
				}
			}
		}
	}
	e.resetReceivers()

	body := `{"n":1}`
	status, reply := e.rawExchange("POST /a/item HTTP/1.1\r\nHost: gw\r\nConnection: close\r\nContent-Type: application/json\r\n"+
		"Transfer-Encoding: chunked\r\nTrailer: X-Trailer\r\n\r\n"+fmt.Sprintf("%x\r\n%s\r\n", len(body), body)+"0\r\nX-Trailer: t\r\n\r\n", false)
	if status != http.StatusOK {
		t.Fatalf("chunked request with a trailer: %d (%s)", status, reply)
	}
	first := e.a.receipts()[0]
	if first.Body != body || first.Headers.Get("Content-Length") != "7" || len(first.Trailer) != 0 || first.Headers.Get("Trailer") != "" {
		t.Errorf("upstream received body %q, Content-Length %q, trailers %v, Trailer field %q",
			first.Body, first.Headers.Get("Content-Length"), first.Trailer, first.Headers.Get("Trailer"))
	}

	// The client receives the gateway's 100 Continue, then the upstream's,
	// relayed, then the final response, as with the legacy runtime.
	resp, conn := e.openResponse("POST /a/item HTTP/1.1\r\nHost: gw\r\nConnection: close\r\nContent-Type: application/json\r\nExpect: 100-continue\r\nContent-Length: 7\r\n\r\n")
	if resp.StatusCode != http.StatusContinue {
		t.Fatalf("interim response %d", resp.StatusCode)
	}
	io.WriteString(conn, body)
	reader := bufio.NewReader(conn)
	interims := 1
	final, err := http.ReadResponse(reader, nil)
	for err == nil && final.StatusCode == http.StatusContinue {
		interims++
		final, err = http.ReadResponse(reader, nil)
	}
	if err != nil || final.StatusCode != http.StatusOK || interims != 2 {
		t.Fatalf("final response %v after %d interim responses: %v", final, interims, err)
	}
	if second := e.a.receipts()[1]; second.Body != body || second.Headers.Get("Expect") != "100-continue" {
		t.Errorf("upstream received body %q with Expect %q", second.Body, second.Headers.Get("Expect"))
	}

	// A declared length together with chunked framing on a connection kept
	// open: whatever net/http makes of it, the upstream receives at most the
	// one validated body, and the bytes after the chunked body are no part of
	// it. If they are read at all, they are a request of their own to the
	// gateway, which refuses GET /d.
	kept, err := net.DialTimeout("tcp", e.dataAddr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer kept.Close()
	kept.SetDeadline(time.Now().Add(10 * time.Second))
	io.WriteString(kept, "POST /a/item HTTP/1.1\r\nHost: gw\r\nContent-Type: application/json\r\nContent-Length: 4\r\nTransfer-Encoding: chunked\r\n\r\n"+
		fmt.Sprintf("%x\r\n%s\r\n0\r\n\r\n", len(body), body)+"GET /d HTTP/1.1\r\nHost: gw\r\nConnection: close\r\n\r\n")
	responses := bufio.NewReader(kept)
	var statuses []int
	for {
		resp, err := http.ReadResponse(responses, nil)
		if err != nil {
			break
		}
		io.Copy(io.Discard, resp.Body)
		statuses = append(statuses, resp.StatusCode)
	}
	got := e.a.receipts()
	t.Logf("declared length with chunked framing: responses %v, %d receipts", statuses, len(got)-2)
	if len(got) > 3 || len(got) == 3 && (got[2].Body != body || got[2].Path != "/a/item" || got[2].Headers.Get("Content-Length") != "7") {
		t.Errorf("upstream received %+v after the two earlier requests", got[2:])
	}
	for _, request := range []string{
		"POST /a/item HTTP/1.1\r\nHost: gw\r\nConnection: close\r\nContent-Type: application/json\r\nContent-Length: 7\r\nContent-Length: 8\r\n\r\n" + body,
		"POST /a/item HTTP/1.1\r\nHost: gw\r\nConnection: close\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\nzz\r\n" + body + "\r\n0\r\n\r\n",
	} {
		before := e.a.count()
		if status, _ := e.rawExchange(request, false); status != http.StatusBadRequest || e.a.count() != before {
			t.Errorf("malformed framing %q: %d with %d new receipts", request, status, e.a.count()-before)
		}
	}
	if e.b.count()+e.c.count() != 0 {
		t.Fatal("a body request reached another upstream")
	}
}

// Responses pass through as the upstream sent them, without the upstream's
// hop-by-hop fields: status, headers and exact body bytes, informational
// responses, redirects (never followed), trailers, streamed parts as they
// arrive, and encoded bodies (never decoded). An upstream that switches
// protocols is not followed into a tunnel.
func TestNativeForwardingResponses(t *testing.T) {
	e := newEnv(t)
	e.serveNative(e.policy(e.a, false), forwardTimeouts)
	var respond func(w http.ResponseWriter, r *http.Request)
	var mu sync.Mutex
	e.a.handle = func(w http.ResponseWriter, r *http.Request) bool {
		mu.Lock()
		f := respond
		mu.Unlock()
		f(w, r)
		return true
	}
	set := func(f func(w http.ResponseWriter, r *http.Request)) {
		mu.Lock()
		respond = f
		mu.Unlock()
	}

	t.Run("status, headers and bytes", func(t *testing.T) {
		binary := make([]byte, 512)
		for i := range binary {
			binary[i] = byte(i)
		}
		set(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("X-Upstream", "kept")
			w.Header().Set("Content-Length", fmt.Sprint(len(binary)))
			w.WriteHeader(http.StatusAccepted)
			w.Write(binary)
		})
		resp, body := e.exchange(http.MethodGet, "/a/x", "", "")
		if resp.StatusCode != http.StatusAccepted || body != string(binary) || resp.ContentLength != int64(len(binary)) || resp.Header.Get("X-Upstream") != "kept" {
			t.Fatalf("response %d, %d bytes (intact %v), headers %v", resp.StatusCode, len(body), body == string(binary), resp.Header)
		}
	})

	t.Run("informational then final", func(t *testing.T) {
		set(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Link", "</style.css>; rel=preload")
			w.WriteHeader(http.StatusEarlyHints)
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, "final")
		})
		resp, conn := e.openResponse("GET /a/x HTTP/1.1\r\nHost: gw\r\nConnection: close\r\n\r\n")
		if resp.StatusCode != http.StatusEarlyHints || resp.Header.Get("Link") == "" {
			t.Fatalf("first response %d %v", resp.StatusCode, resp.Header)
		}
		final, err := http.ReadResponse(bufio.NewReader(io.MultiReader(resp.Body, conn)), nil)
		if err != nil || final.StatusCode != http.StatusOK {
			t.Fatalf("final response %v: %v", final, err)
		}
	})

	t.Run("redirect not followed", func(t *testing.T) {
		set(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://"+e.c.addr+"/a/x", http.StatusFound)
		})
		resp, _ := e.rawResponse("GET /a/x HTTP/1.1\r\nHost: gw\r\nConnection: close\r\n\r\n")
		if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "http://"+e.c.addr+"/a/x" || e.c.count() != 0 {
			t.Fatalf("redirect: %d, Location %q, receipts at C %d", resp.StatusCode, resp.Header.Get("Location"), e.c.count())
		}
	})

	t.Run("trailers", func(t *testing.T) {
		set(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Trailer", "X-Checksum")
			io.WriteString(w, "body")
			w.Header().Set("X-Checksum", "c0ffee")
		})
		resp, body := e.exchange(http.MethodGet, "/a/x", "", "")
		if resp.StatusCode != http.StatusOK || body != "body" || resp.Trailer.Get("X-Checksum") != "c0ffee" {
			t.Fatalf("response %d %q, trailers %v", resp.StatusCode, body, resp.Trailer)
		}
	})

	t.Run("streamed as it arrives", func(t *testing.T) {
		next := make(chan struct{})
		set(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, "first\n")
			w.(http.Flusher).Flush()
			select {
			case <-next:
			case <-time.After(10 * time.Second):
			}
			io.WriteString(w, "second\n")
		})
		resp, _ := e.openResponse("GET /a/x HTTP/1.1\r\nHost: gw\r\nConnection: close\r\n\r\n")
		reader := bufio.NewReader(resp.Body)
		if line, err := reader.ReadString('\n'); err != nil || line != "first\n" {
			t.Fatalf("first part: %q %v", line, err)
		}
		close(next)
		if rest, err := io.ReadAll(reader); err != nil || string(rest) != "second\n" {
			t.Fatalf("second part: %q %v", rest, err)
		}
	})

	t.Run("encoded body not decoded", func(t *testing.T) {
		var compressed bytes.Buffer
		zw := gzip.NewWriter(&compressed)
		io.WriteString(zw, strings.Repeat("payload ", 64))
		zw.Close()
		set(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Accept-Encoding") != "gzip" {
				w.WriteHeader(http.StatusNotAcceptable)
				return
			}
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Set("Content-Length", fmt.Sprint(compressed.Len()))
			w.Write(compressed.Bytes())
		})
		resp, body := e.rawResponse("GET /a/x HTTP/1.1\r\nHost: gw\r\nAccept-Encoding: gzip\r\nConnection: close\r\n\r\n")
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Encoding") != "gzip" || body != compressed.String() ||
			resp.ContentLength != int64(compressed.Len()) {
			t.Fatalf("response %d, Content-Encoding %q, %d bytes (identical %v)", resp.StatusCode, resp.Header.Get("Content-Encoding"), len(body), body == compressed.String())
		}
	})

	t.Run("protocol switch refused", func(t *testing.T) {
		closed := make(chan struct{})
		set(func(w http.ResponseWriter, r *http.Request) {
			conn, buffered, _ := http.NewResponseController(w).Hijack()
			defer conn.Close()
			buffered.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
			buffered.Flush()
			conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			if _, err := buffered.ReadByte(); errors.Is(err, io.EOF) {
				close(closed)
			}
		})
		resp, body := e.rawResponse("GET /a/x HTTP/1.1\r\nHost: gw\r\nConnection: close\r\n\r\n")
		_, id := requestID(resp, body)
		if resp.StatusCode != http.StatusBadGateway || !failedOutcome(e.outcomeOf(id), http.StatusBadGateway, "upstream_unavailable", "outcome_unknown") {
			t.Fatalf("upstream switching protocols: %d %s, outcome %v", resp.StatusCode, body, e.outcomeOf(id))
		}
		select {
		case <-closed:
		case <-time.After(5 * time.Second):
			t.Fatal("the gateway kept the switched upstream connection open")
		}
	})
}

// Failures before any response are answered by the gateway with a bounded,
// safe error and recorded with what is known about delivery: "not_sent" when
// no connection to the upstream existed, "outcome_unknown" when the upstream
// may have acted on the request, and "client_cancelled" when the client went
// away. Nothing is resent and no other destination is tried.
func TestNativeForwardingFailuresBeforeResponse(t *testing.T) {
	t.Run("connection refused", func(t *testing.T) {
		e := newEnv(t)
		closed := freeAddr(t)
		e.serveNative(e.policyWithUpstream(closed), forwardTimeouts)
		resp, body := e.exchange(http.MethodGet, "/z", "", "")
		_, id := requestID(resp, body)
		if resp.StatusCode != http.StatusBadGateway || strings.Contains(body, port(closed)) ||
			!failedOutcome(e.outcomeOf(id), http.StatusBadGateway, "upstream_unavailable", "not_sent") ||
			!sameStages(e.outcomeOf(id)["stages"], publicAdmitted(stageFail)) {
			t.Fatalf("refused upstream: %d %s, outcome %v", resp.StatusCode, body, e.outcomeOf(id))
		}
		e.assertReceipts(0, 0, 0)
	})

	t.Run("upstream that never accepts", func(t *testing.T) {
		e := newEnv(t)
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		timeouts := forwardTimeouts
		timeouts.responseHeader = 500 * time.Millisecond
		e.serveNative(e.policyWithUpstream(listener.Addr().String()), timeouts)
		// The connection completes in the listener's queue, so the request
		// may have been delivered as far as the gateway can tell.
		resp, body := e.exchange(http.MethodGet, "/z", "", "")
		_, id := requestID(resp, body)
		if resp.StatusCode != http.StatusGatewayTimeout || !failedOutcome(e.outcomeOf(id), http.StatusGatewayTimeout, "upstream_timeout", "outcome_unknown") {
			t.Fatalf("upstream that never accepts: %d %s, outcome %v", resp.StatusCode, body, e.outcomeOf(id))
		}
	})

	stagedFailures := []struct {
		name   string
		handle func(w http.ResponseWriter, r *http.Request) bool
		status int
		code   string
	}{
		{"closed before the response", func(w http.ResponseWriter, r *http.Request) bool {
			conn, _, _ := http.NewResponseController(w).Hijack()
			conn.Close()
			return true
		}, http.StatusBadGateway, "upstream_unavailable"},
		{"malformed response", func(w http.ResponseWriter, r *http.Request) bool {
			conn, buffered, _ := http.NewResponseController(w).Hijack()
			buffered.WriteString("this is not HTTP\r\n\r\n")
			buffered.Flush()
			conn.Close()
			return true
		}, http.StatusBadGateway, "upstream_unavailable"},
	}
	for _, tc := range stagedFailures {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.serveNative(e.policy(e.a, false), forwardTimeouts)
			e.a.handle = tc.handle
			resp, body := e.exchange(http.MethodGet, "/a/x", "", "")
			_, id := requestID(resp, body)
			if resp.StatusCode != tc.status || !strings.Contains(body, `"error":"`+tc.code+`"`) || strings.Contains(body, "HTTP") ||
				!failedOutcome(e.outcomeOf(id), tc.status, tc.code, "outcome_unknown") {
				t.Fatalf("%d %s, outcome %v", resp.StatusCode, body, e.outcomeOf(id))
			}
			e.assertReceipts(1, 0, 0)
		})
	}

	t.Run("response-header timeout", func(t *testing.T) {
		e := newEnv(t)
		timeouts := forwardTimeouts
		timeouts.responseHeader = 500 * time.Millisecond
		e.serveNative(e.policy(e.a, false), timeouts)
		cancelled := make(chan struct{})
		e.a.handle = func(w http.ResponseWriter, r *http.Request) bool {
			select {
			case <-r.Context().Done():
				close(cancelled)
			case <-time.After(10 * time.Second):
			}
			return true
		}
		started := time.Now()
		resp, body := e.exchange(http.MethodGet, "/a/x", "", "")
		elapsed := time.Since(started)
		_, id := requestID(resp, body)
		if resp.StatusCode != http.StatusGatewayTimeout || elapsed > 3*time.Second ||
			!failedOutcome(e.outcomeOf(id), http.StatusGatewayTimeout, "upstream_timeout", "outcome_unknown") {
			t.Fatalf("slow upstream: %d %s after %v, outcome %v", resp.StatusCode, body, elapsed, e.outcomeOf(id))
		}
		select {
		case <-cancelled:
		case <-time.After(5 * time.Second):
			t.Fatal("the upstream request was not cancelled")
		}
		// The request did arrive; the gateway cannot undo what the upstream did.
		e.assertReceipts(1, 0, 0)
	})

	t.Run("client cancels", func(t *testing.T) {
		e := newEnv(t)
		e.serveNative(e.policy(e.a, false), forwardTimeouts)
		received, cancelled := make(chan struct{}), make(chan struct{})
		e.a.handle = func(w http.ResponseWriter, r *http.Request) bool {
			close(received)
			select {
			case <-r.Context().Done():
				close(cancelled)
			case <-time.After(30 * time.Second):
			}
			return true
		}
		conn, err := net.DialTimeout("tcp", e.dataAddr, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(conn, "GET /a/x HTTP/1.1\r\nHost: gw\r\n\r\n")
		<-received
		conn.Close()
		select {
		case <-cancelled:
		case <-time.After(5 * time.Second):
			t.Fatal("the client's cancellation did not reach the upstream")
		}
		ev := e.outcomeOf(e.a.receipts()[0].RequestID)
		if !failedOutcome(ev, http.StatusBadGateway, "upstream_unavailable", "client_cancelled") || !sameStages(ev["stages"], publicAdmitted(stageFail)) {
			t.Fatalf("outcome %v", ev)
		}
		e.assertReceipts(1, 0, 0)
	})
}

// The gateway's answer and the delivery reason for each way an exchange can
// fail before any response, including a dial timeout, which a loopback
// upstream cannot produce on every platform: an unanswered connection
// attempt is refused at once on Windows even when the listener's queue is
// full. The dial errors are the real dialer's.
func TestNativeForwardingFailureClassification(t *testing.T) {
	addr := freeAddr(t)
	_, refused := net.Dial("tcp", addr)
	_, timedOut := (&net.Dialer{Deadline: time.Now().Add(-time.Second)}).Dial("tcp", addr)
	if refused == nil || timedOut == nil {
		t.Fatalf("dial errors: %v, %v", refused, timedOut)
	}
	if netErr, ok := errors.AsType[net.Error](timedOut); !ok || !netErr.Timeout() {
		t.Fatalf("the expired dial did not time out: %v", timedOut)
	}
	headerTimeout := &url.Error{Op: "Get", URL: "http://" + addr, Err: timeoutError{}}
	cases := []struct {
		name                  string
		err                   error
		connected, clientGone bool
		status                int
		code, reason          string
	}{
		{"connection refused", refused, false, false, 502, "upstream_unavailable", "not_sent"},
		{"dial timeout", timedOut, false, false, 502, "upstream_unavailable", "not_sent"},
		{"client left before a connection", context.Canceled, false, true, 502, "upstream_unavailable", "not_sent"},
		{"client left once connected", context.Canceled, true, true, 502, "upstream_unavailable", "client_cancelled"},
		{"connection closed", io.EOF, true, false, 502, "upstream_unavailable", "outcome_unknown"},
		{"response-header timeout", headerTimeout, true, false, 504, "upstream_timeout", "outcome_unknown"},
	}
	for _, tc := range cases {
		err := exchangeFailure(tc.err, tc.connected, tc.clientGone)
		status, code := forwardFailure(err)
		if status != tc.status || code != tc.code || failureReason(err) != tc.reason || !errors.Is(err, tc.err) {
			t.Errorf("%s: %v gives %d %s (%s), want %d %s (%s)", tc.name, err, status, code, failureReason(err), tc.status, tc.code, tc.reason)
		}
	}
}

// timeoutError is a net.Error that timed out, as the transport's
// response-header timeout error is.
type timeoutError struct{}

func (timeoutError) Error() string   { return "timeout awaiting response headers" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

// A request whose response is lost is sent once only. A write the upstream
// applied before closing the connection is applied once and recorded as of
// unknown outcome; reads and body-less writes carrying an Idempotency-Key,
// which a transport with idle connections would resend after a reused
// connection fails, are not resent either.
func TestNativeForwardingNeverResends(t *testing.T) {
	e := newEnv(t)
	p := e.policy(e.a, false)
	p.Operations = append(p.Operations, Operation{ID: "remove-a", API: "a", Method: "DELETE", Target: "/a/item", Access: "allow", Auth: "none"})
	e.serveNative(p, forwardTimeouts)
	var applied atomic.Int64 // the upstream's own record of writes it carried out
	e.a.handle = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/a/warm" {
			return false
		}
		if r.Method != http.MethodGet {
			applied.Add(1)
		}
		// Applied, then the response is lost.
		conn, _, _ := http.NewResponseController(w).Hijack()
		conn.Close()
		return true
	}
	requests := []struct {
		name, method, path, ctype, body string
		header                          []string
		writes                          int64
	}{
		{"write with a body", http.MethodPost, "/a/item", "application/json", `{"n":1}`, []string{"Idempotency-Key", "write-0001"}, 1},
		{"read", http.MethodGet, "/a/lost", "", "", nil, 0},
		{"body-less write with an idempotency key", http.MethodDelete, "/a/item", "", "", []string{"Idempotency-Key", "delete-0001"}, 1},
	}
	for _, tc := range requests {
		// A completed exchange first: a transport keeping idle connections
		// would send the next request on the same connection.
		if code, body := e.get("/a/warm"); code != http.StatusOK || body != "A ok" {
			t.Fatalf("%s: warm-up %d %q", tc.name, code, body)
		}
		received, writes := e.a.count(), applied.Load()
		resp, body := e.exchange(tc.method, tc.path, tc.ctype, tc.body, tc.header...)
		_, id := requestID(resp, body)
		if resp.StatusCode != http.StatusBadGateway || !failedOutcome(e.outcomeOf(id), http.StatusBadGateway, "upstream_unavailable", "outcome_unknown") {
			t.Errorf("%s: %d %s, outcome %v", tc.name, resp.StatusCode, body, e.outcomeOf(id))
		}
		if got, done := e.a.count()-received, applied.Load()-writes; got != 1 || done != tc.writes {
			t.Errorf("%s: the upstream received it %d times and applied %d writes, want once and %d", tc.name, got, done, tc.writes)
		}
	}
}

// After the response header, the read-idle bound ends an upstream body that
// stops arriving: the client receives what came and then an aborted
// response, never a later complete or substituted one; the upstream request is
// cancelled; the outcome is an incomplete response. A body that keeps
// arriving more slowly than the bound in total, but never pauses for as long,
// completes; a pause before the header is bounded by the header timeout
// instead. A client that leaves during a response ends the upstream request.
func TestNativeForwardingResponseStalls(t *testing.T) {
	const idle = 500 * time.Millisecond
	timeouts := forwardTimeouts
	timeouts.readIdle = idle
	e := newEnv(t)
	_, logs := e.serveNative(e.policy(e.a, false), timeouts)
	var respond func(w http.ResponseWriter, r *http.Request)
	var mu sync.Mutex
	e.a.handle = func(w http.ResponseWriter, r *http.Request) bool {
		mu.Lock()
		f := respond
		mu.Unlock()
		f(w, r)
		return true
	}
	set := func(f func(w http.ResponseWriter, r *http.Request)) {
		mu.Lock()
		respond = f
		mu.Unlock()
	}
	lastOutcome := func() map[string]any {
		got := e.a.receipts()
		return e.outcomeOf(got[len(got)-1].RequestID)
	}

	t.Run("stall after the header", func(t *testing.T) {
		cancelled := make(chan time.Time, 1)
		set(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, "partial ")
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				cancelled <- time.Now()
			case <-time.After(30 * time.Second):
			}
		})
		resp, _ := e.openResponse("GET /a/x HTTP/1.1\r\nHost: gw\r\n\r\n")
		first := make([]byte, len("partial "))
		if _, err := io.ReadFull(resp.Body, first); err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("response %d, first bytes %q: %v", resp.StatusCode, first, err)
		}
		stalled := time.Now()
		rest, err := io.ReadAll(resp.Body)
		ended := time.Since(stalled)
		if err == nil || len(rest) != 0 || ended < idle-100*time.Millisecond || ended > 3*time.Second {
			t.Fatalf("after the stall: %q, %v, after %v (bound %v)", rest, err, ended, idle)
		}
		select {
		case at := <-cancelled:
			t.Logf("client saw the abort %v after the stall began; the upstream request was cancelled %v after it", ended, at.Sub(stalled))
		case <-time.After(5 * time.Second):
			t.Fatal("the stalled upstream request was not cancelled")
		}
		if ev := lastOutcome(); !failedOutcome(ev, http.StatusOK, "upstream_incomplete", "") {
			t.Fatalf("outcome %v", ev)
		}
		if !strings.Contains(logs.String(), errResponseStalled.Error()) {
			t.Fatalf("proxy log %s", logs)
		}
	})

	t.Run("slow but progressing", func(t *testing.T) {
		set(func(w http.ResponseWriter, r *http.Request) {
			for i := range 12 {
				fmt.Fprint(w, i%10)
				w.(http.Flusher).Flush()
				time.Sleep(idle * 2 / 5)
			}
		})
		started := time.Now()
		resp, body := e.exchange(http.MethodGet, "/a/x", "", "")
		elapsed := time.Since(started)
		if resp.StatusCode != http.StatusOK || body != "012345678901" || elapsed < 4*idle {
			t.Fatalf("slow response: %d %q after %v", resp.StatusCode, body, elapsed)
		}
		if ev := lastOutcome(); ev["decision"] != "forwarded" || ev["status"] != float64(200) {
			t.Fatalf("outcome %v", ev)
		}
	})

	t.Run("pause before the header", func(t *testing.T) {
		set(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(3 * idle)
			io.WriteString(w, "late")
		})
		if code, body := e.get("/a/x"); code != http.StatusOK || body != "late" {
			t.Fatalf("pause before the header: %d %q", code, body)
		}
	})

	t.Run("partial then closed", func(t *testing.T) {
		// The upstream declares twice the bytes it sends. A response the
		// gateway had only begun buffering never reaches the client; a longer
		// one arrives in part and then the connection closes. Neither ends
		// as a complete response.
		for _, sent := range []int{10, 16 << 10} {
			set(func(w http.ResponseWriter, r *http.Request) {
				conn, buffered, _ := http.NewResponseController(w).Hijack()
				fmt.Fprintf(buffered, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n%s", 2*sent, strings.Repeat("x", sent))
				buffered.Flush()
				conn.Close()
			})
			conn, err := net.DialTimeout("tcp", e.dataAddr, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			conn.SetDeadline(time.Now().Add(10 * time.Second))
			io.WriteString(conn, "GET /a/x HTTP/1.1\r\nHost: gw\r\n\r\n")
			resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				t.Logf("%d of %d bytes sent: the client received no response (%v)", sent, 2*sent, err)
			} else {
				received, err := io.ReadAll(resp.Body)
				if resp.StatusCode != http.StatusOK || !errors.Is(err, io.ErrUnexpectedEOF) || len(received) > sent {
					t.Fatalf("%d of %d bytes sent: client received %d %d bytes, %v", sent, 2*sent, resp.StatusCode, len(received), err)
				}
				t.Logf("%d of %d bytes sent: the client received status %d and %d bytes, then %v", sent, 2*sent, resp.StatusCode, len(received), err)
			}
			conn.Close()
			if ev := lastOutcome(); !failedOutcome(ev, http.StatusOK, "upstream_incomplete", "") {
				t.Fatalf("%d bytes sent: outcome %v", sent, ev)
			}
		}
	})

	t.Run("client leaves during the response", func(t *testing.T) {
		cancelled := make(chan struct{})
		set(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, "first")
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				close(cancelled)
			case <-time.After(30 * time.Second):
			}
		})
		resp, conn := e.openResponse("GET /a/x HTTP/1.1\r\nHost: gw\r\n\r\n")
		first := make([]byte, len("first"))
		if _, err := io.ReadFull(resp.Body, first); err != nil {
			t.Fatal(err)
		}
		conn.Close()
		select {
		case <-cancelled:
		case <-time.After(idle / 2):
			t.Fatal("the upstream request outlived the client")
		}
		if ev := lastOutcome(); !failedOutcome(ev, http.StatusOK, "upstream_incomplete", "") {
			t.Fatalf("outcome %v", ev)
		}
	})
}

// Under load, every stalled or abandoned exchange ends within its bound and
// releases its upstream request and connection: no transport goroutine is
// left once the clients are done.
func TestNativeForwardingReleasesStalledExchanges(t *testing.T) {
	const transportGoroutine = "net/http.(*persistConn)."
	e := newEnv(t)
	timeouts := forwardTimeouts
	timeouts.readIdle = 300 * time.Millisecond
	e.serveNative(e.policy(e.a, false), timeouts)
	before := goroutines(transportGoroutine)
	var cancelled atomic.Int64
	e.a.handle = func(w http.ResponseWriter, r *http.Request) bool {
		io.WriteString(w, "partial")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			cancelled.Add(1)
		case <-time.After(30 * time.Second):
		}
		return true
	}
	const stalled, abandoned = 16, 8
	started := time.Now()
	var clients sync.WaitGroup
	for i := range stalled + abandoned {
		clients.Go(func() {
			conn, err := net.DialTimeout("tcp", e.dataAddr, 5*time.Second)
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(20 * time.Second))
			io.WriteString(conn, "GET /a/x HTTP/1.1\r\nHost: gw\r\n\r\n")
			resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				t.Error(err)
				return
			}
			first := make([]byte, len("partial"))
			if _, err := io.ReadFull(resp.Body, first); err != nil {
				t.Error(err)
				return
			}
			if i >= stalled {
				return // the client leaves
			}
			if _, err := io.ReadAll(resp.Body); err == nil {
				t.Error("a stalled response was completed")
			}
		})
	}
	clients.Wait()
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Errorf("stalled exchanges ended after %v", elapsed)
	}
	waitFor(t, time.Now().Add(5*time.Second), "every upstream request to be cancelled", func() bool { return cancelled.Load() == stalled+abandoned })
	waitGoroutines(t, transportGoroutine, before)
	incomplete := 0
	for _, ev := range e.eventsWhen(func(events []map[string]any) bool { return countKind(events, "request.forwarded") == stalled+abandoned }) {
		if ev["kind"] == "request.forwarded" && ev["error"] == "upstream_incomplete" {
			incomplete++
		}
	}
	if incomplete != stalled+abandoned {
		t.Fatalf("%d incomplete outcomes, want %d", incomplete, stalled+abandoned)
	}
	e.assertReceipts(stalled+abandoned, 0, 0)
}

// Every refusal by the five controls or the audit gate happens before the
// native proxy runs: no upstream receives the request, and the admitted
// requests that follow do reach it.
func TestNativeForwardingRefusalsReachNoUpstream(t *testing.T) {
	e := newEnv(t)
	p := e.protectedPolicy()
	p.Operations = append(p.Operations,
		Operation{ID: "strict", API: "a", Method: "POST", Target: "/a/strict", Access: "allow", Auth: "required",
			Body: jsonBody(1024, `{"type":"object","additionalProperties":true,"required":["n"],"properties":{"n":{"type":"integer"}}}`)},
		Operation{ID: "limited", API: "a", Method: "GET", Target: "/a/limited", Access: "allow", Auth: "required", Rate: rate(3, 60)},
		Operation{ID: "probe", API: "b", Method: "GET", Target: "/b/probe", Access: "allow", Auth: "none",
			Headers: json.RawMessage(`{"type":"object","additionalProperties":true,"required":["X-Probe"],"properties":{"X-Probe":{"type":"string","enum":["on"]}}}`)},
	)
	p.Permits = append(p.Permits, Permit{Role: "WRITER", Operation: "strict"}, Permit{Role: "WRITER", Operation: "limited"})
	c, logs := e.serveNative(p, forwardTimeouts)
	token := issuer.tokenFor(t, "alice")
	segments := strings.Split(token, ".")
	refusals := []struct {
		name   string
		method string
		path   string
		ctype  string
		body   string
		header []string
		status int
		stage  string
	}{
		{"no token", "GET", "/a/x", "", "", nil, 401, "JWT"},
		{"altered signature", "GET", "/a/x", "", "", []string{"Authorization", "Bearer " + segments[0] + "." + segments[1] + ".AAAA" + segments[2][4:]}, 401, "JWT"},
		{"subject without a permit", "POST", "/a/item", "application/json", `{"n":1}`, bearer(t, "bob"), 403, "RBAC"},
		{"unknown role claimed in the verified token", "POST", "/a/item", "application/json", `{"n":1}`,
			[]string{"Authorization", "Bearer " + issuer.token(t, changed(validClaims("bob"), map[string]any{"roles": []string{"ADMIN"}}))}, 403, "RBAC"},
		{"media type", "POST", "/a/strict", "text/plain", `{"n":1}`, []string{"Authorization", "Bearer " + token}, 415, "BODY"},
		{"malformed JSON", "POST", "/a/strict", "application/json", `{"n":`, []string{"Authorization", "Bearer " + token}, 400, "BODY"},
		{"body schema", "POST", "/a/strict", "application/json", `{"n":"one"}`, []string{"Authorization", "Bearer " + token}, 400, "VALIDATION"},
		{"header schema", "GET", "/b/probe", "", "", []string{"X-Probe", "off"}, 400, "VALIDATION"},
		{"Connection naming a gateway header", "GET", "/a/x", "", "", []string{"Authorization", "Bearer " + token, "Connection", "X-Gateway-Subject"}, 400, "INGRESS"},
	}
	for _, tc := range refusals {
		resp, body := e.exchange(tc.method, tc.path, tc.ctype, tc.body, tc.header...)
		_, id := requestID(resp, body)
		if ev := e.eventOf(id, 1); resp.StatusCode != tc.status || ev["stage"] != tc.stage {
			t.Errorf("%s: %d %s, event %v, want %d at %s", tc.name, resp.StatusCode, body, ev, tc.status, tc.stage)
		}
		e.assertReceipts(0, 0, 0)
	}

	// The rate rule admits three requests in its window.
	for i := range 4 {
		code, _ := e.get("/a/limited", "Authorization", "Bearer "+token)
		if want := map[bool]int{true: http.StatusOK, false: http.StatusTooManyRequests}[i < 3]; code != want {
			t.Fatalf("rated request %d: %d, want %d", i+1, code, want)
		}
	}
	e.assertReceipts(3, 0, 0)

	// An audit log that cannot record the admission admits nothing.
	c.audit.fail(errors.New("injected write failure"))
	if code, body := e.get("/a/x", "Authorization", "Bearer "+token); code != http.StatusServiceUnavailable || !strings.Contains(body, "gateway_unavailable") {
		t.Fatalf("with a failed audit log: %d %s", code, body)
	}
	e.assertReceipts(3, 0, 0)
	for name, text := range map[string]string{"audit log": fmt.Sprint(e.auditEvents()), "proxy log": logs.String()} {
		if strings.Contains(text, token) || strings.Contains(text, segments[2]) {
			t.Errorf("the %s holds the token", name)
		}
	}
	for _, r := range e.a.receipts() {
		if r.Authorization {
			t.Error("the credential reached the upstream")
		}
	}
}
