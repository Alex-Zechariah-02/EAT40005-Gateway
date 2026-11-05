// Forwarding helpers, setup, fixtures and bound destinations.

package gateway

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/forward"
	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

// These tests serve the enforcement handler with the native reverse proxy as
// its forwarder, under a plain net/http server in the test process, and
// forward to real loopback upstreams: the deployment's receivers, the three
// fixture processes, or a receiver whose handler stages a failure. What an
// upstream received, or that it received nothing, is read from the upstream
// itself.

// forwardTimeouts are the legacy dial and response-header bounds and the
// read-idle bound of the native transport.
var forwardTimeouts = forward.UpstreamTimeouts{Dial: 2 * time.Second, ResponseHeader: 5 * time.Second, ReadIdle: 5 * time.Second}

// serveNative seeds e's database with policy, owns and starts its controller,
// and serves the enforcement handler with the native proxy, bounded by
// timeouts, at e.dataAddr, where the harness request helpers send. It returns
// the controller and the proxy's log.
func (e *env) serveNative(policy policy.Policy, timeouts forward.UpstreamTimeouts) (*Controller, *logBuffer) {
	e.t.Helper()
	seed(e.t, e.database, policy, seedOptions{})
	c, _ := e.own(e.controllerConfig())
	if err := c.Start(); err != nil {
		e.t.Fatal(err)
	}
	logs := &logBuffer{}
	p, err := forward.NewUpstreamProxy(timeouts, slog.New(slog.NewJSONHandler(logs, nil)))
	if err != nil {
		e.t.Fatal(err)
	}
	h, err := newHandler(c, 0, p.Forward)
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
