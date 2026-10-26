package gateway

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAdmittedRequestReachesOnlyBoundUpstream(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.policy(e.a, false), seedOptions{})
	e.start(binaries.plain)
	g := e.status()

	if code, body := e.get("/a/x"); code != http.StatusOK || body != "A ok" {
		t.Fatalf("admitted request: %d %q", code, body)
	}
	e.assertReceipts(1, 0, 0)
	if got := e.a.receipts()[0]; got.Generation != g.Generation || got.Target != "/a/x" {
		t.Fatalf("receipt %+v, want generation %s", got, g.Generation)
	}

	if code, _ := e.get("/d"); code != http.StatusForbidden {
		t.Fatalf("denied operation: %d", code)
	}
	if code, _ := e.get("/zzz"); code != http.StatusNotFound {
		t.Fatalf("unknown operation: %d", code)
	}
	e.assertReceipts(1, 0, 0)

	events := e.eventsWhen(func(events []map[string]any) bool { return countKind(events, "request.rejected") >= 2 })
	admitted, rejected := countKind(events, "request.admitted"), countKind(events, "request.rejected")
	if admitted != 1 || rejected != 2 {
		t.Fatalf("audit admitted=%d rejected=%d, want 1 and 2", admitted, rejected)
	}
}

func TestPublicationAddsOperationWithoutReload(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.policy(e.a, false), seedOptions{})
	configHash := fileSHA256(t, e.config)
	e.start(binaries.fault)
	pid := e.process.Process.Pid

	if code, _ := e.get("/b/x"); code != http.StatusNotFound {
		t.Fatalf("before publication: %d", code)
	}
	outcome, code := e.publish(Change{ID: "add-b", Expected: 1, Policy: mustJSON(e.policy(e.a, true))})
	if code != http.StatusOK || outcome.Revision != 2 {
		t.Fatalf("publish: %d %+v", code, outcome)
	}
	if code, body := e.get("/b/x"); code != http.StatusOK || body != "B ok" {
		t.Fatalf("new operation: %d %q", code, body)
	}
	if code, body := e.get("/a/x"); code != http.StatusOK || body != "A ok" {
		t.Fatalf("existing operation: %d %q", code, body)
	}
	e.assertReceipts(1, 1, 0)
	for _, r := range append(e.a.receipts(), e.b.receipts()...) {
		if r.Generation != outcome.Generation {
			t.Fatalf("receipt generation %s, want %s", r.Generation, outcome.Generation)
		}
	}
	if e.process.Process.Pid != pid || fileSHA256(t, e.config) != configHash {
		t.Fatal("gateway process or configuration changed")
	}
}

// holdRequest sends the headers and half of a body for POST /a/item, then
// waits until the gateway signals that the request captured its generation.
// A non-empty token is sent as the bearer credential.
func holdRequest(t *testing.T, e *env, holdFile, token string) (net.Conn, string) {
	t.Helper()
	conn, err := net.Dial("tcp", e.dataAddr)
	if err != nil {
		t.Fatal(err)
	}
	credential := ""
	if token != "" {
		credential = "Authorization: Bearer " + token + "\r\n"
	}
	fmt.Fprintf(conn, "POST /a/item HTTP/1.1\r\nHost: gw\r\n%sContent-Type: application/json\r\nContent-Length: 13\r\nConnection: close\r\n\r\n{\"half\":", credential)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(holdFile); err == nil && len(data) > 0 {
			return conn, strings.TrimSpace(string(data))
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("request never captured a generation")
	return nil, ""
}

func TestInFlightRequestKeepsCapturedGeneration(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.policy(e.a, false), seedOptions{})
	holdFile := filepath.Join(e.dir, "hold")
	e.start(binaries.fault, "GATEWAY_HOLD_PATH=/a/item", "GATEWAY_HOLD_FILE="+holdFile)
	first := e.status().Generation

	conn, captured := holdRequest(t, e, holdFile, "")
	defer conn.Close()
	if captured != first {
		t.Fatalf("captured %s, want %s", captured, first)
	}
	outcome, code := e.publish(Change{ID: "move-a", Expected: 1, Policy: mustJSON(e.policy(e.b, false))})
	if code != http.StatusOK {
		t.Fatalf("publish: %d %+v", code, outcome)
	}

	io.WriteString(conn, "true}")
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("in-flight request: %v %v", resp, err)
	}
	e.assertReceipts(1, 0, 0)
	if got := e.a.receipts()[0].Generation; got != first {
		t.Fatalf("in-flight request reached A under %s, want %s", got, first)
	}

	if code, body := e.request(http.MethodPost, "/a/item", "application/json", `{"n":1}`); code != http.StatusOK || body != "B ok" {
		t.Fatalf("new request: %d %q", code, body)
	}
	e.assertReceipts(1, 1, 0)
	if got := e.b.receipts()[0].Generation; got != outcome.Generation {
		t.Fatalf("new request reached B under %s, want %s", got, outcome.Generation)
	}

	// The events agree with the receipts: each request was matched, bound
	// and recorded under the generation it captured.
	held, later := e.a.receipts()[0].RequestID, e.b.receipts()[0].RequestID
	events := e.eventsWhen(func(events []map[string]any) bool { return countKind(events, "request.forwarded") == 2 })
	for _, want := range []struct{ id, generation, upstream string }{{held, first, "A"}, {later, outcome.Generation, "B"}} {
		ev := findEvent(events, "request.forwarded", want.id)
		if ev == nil || ev["generation"] != want.generation || ev["upstream"] != want.upstream || ev["operation"] != "write-a" {
			t.Fatalf("event %v, want generation %s upstream %s", ev, want.generation, want.upstream)
		}
	}
}

// A request whose exchange fails while a publication moves its operation to
// another upstream is answered 502 and was sent once, to the destination of
// the generation it captured; it is never sent again, there or elsewhere.
func TestFailedAttemptStaysOnCapturedDestination(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.policy(e.a, false), seedOptions{})
	e.start(binaries.fault)
	captured := e.status().Generation

	published := make(chan struct{})
	var once sync.Once
	e.a.handle = func(w http.ResponseWriter, r *http.Request) bool {
		once.Do(func() {
			outcome, code := e.publish(Change{ID: "move-a", Expected: 1, Policy: mustJSON(e.policy(e.b, false))})
			if code != http.StatusOK {
				t.Errorf("publish during attempt: %d %+v", code, outcome)
			}
			close(published)
		})
		// Drop the connection without a response: a transport error after
		// the request was sent.
		conn, _, _ := http.NewResponseController(w).Hijack()
		conn.Close()
		return true
	}
	code, _ := e.get("/a/x")
	<-published
	if code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", code)
	}
	e.assertReceipts(1, 0, 0)
	if g := e.a.receipts()[0].Generation; g != captured {
		t.Fatalf("attempt under generation %s, want %s", g, captured)
	}
}

func TestCanonicalRoutingTable(t *testing.T) {
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
	seed(t, e.database, policy, seedOptions{})
	e.start(binaries.plain)

	type row struct {
		request string
		status  int
		to      *receiver
		target  string // expected raw target at the receiver
		path    string // expected decoded path at the receiver
	}
	rows := []row{
		{"GET /orders/42", 200, e.a, "/orders/42", "/orders/42"},
		{"GET /orders", 405, nil, "", ""},
		{"GET /orders/", 404, nil, "", ""},
		{"GET /Orders/42", 404, nil, "", ""},
		{"GET /orders/42/items", 200, e.a, "/orders/42/items", "/orders/42/items"},
		{"GET /ordersX", 404, nil, "", ""},
		{"GET /orders/%2F42", 400, nil, "", ""},
		{"GET /orders/../reports", 400, nil, "", ""},
		{"GET /orders/%2e%2e/reports", 400, nil, "", ""},
		{"GET /orders//42", 400, nil, "", ""},
		{"GET /orders/42?x=1&y=2", 200, e.a, "/orders/42?x=1&y=2", "/orders/42"},
		{"get /orders/42", 405, nil, "", ""},
		{"HEAD /orders/42", 405, nil, "", ""},
		{"OPTIONS /orders/42", 405, nil, "", ""},
		// Refused at ingress, like every target that is not in origin form.
		{"OPTIONS *", 400, nil, "", ""},
		{"GET /orders/%41", 200, e.a, "/orders/%41", "/orders/A"},
		{"GET /orders/%252F", 400, nil, "", ""},
		{"GET /reports/", 404, nil, "", ""},
		{"DELETE /orders/archive", 403, nil, "", ""},
		{"GET /orders/archive", 405, nil, "", ""},
		{"GET /orders/a%5Cb", 400, nil, "", ""},
		{"GET http://x/orders/42", 400, nil, "", ""},
		{"PATCH /unknown", 404, nil, "", ""},
		{"GET /reports", 200, e.b, "/reports", "/reports"},
	}
	for _, r := range rows {
		e.resetReceivers()
		code, _ := e.raw(r.request + " HTTP/1.1\r\nHost: gw\r\nConnection: close\r\n\r\n")
		if code != r.status {
			t.Errorf("%s: status %d, want %d", r.request, code, r.status)
			continue
		}
		total := e.a.count() + e.b.count() + e.c.count()
		if r.to == nil {
			if total != 0 {
				t.Errorf("%s: rejected request reached an upstream", r.request)
			}
			continue
		}
		got := r.to.receipts()
		if total != 1 || len(got) != 1 || got[0].Target != r.target || got[0].Path != r.path {
			t.Errorf("%s: receipts %+v (total %d), want one at %s target %q path %q", r.request, got, total, r.to.name, r.target, r.path)
		}
	}

	bodies := []struct {
		name        string
		contentType string
		body        string
		status      int
	}{
		{"valid JSON", "application/json", `{"item":1}`, 200},
		{"oversize", "application/json", `{"x":"` + strings.Repeat("a", 2000) + `"}`, 413},
		{"wrong media type", "text/plain", `{"item":1}`, 415},
		{"malformed JSON", "application/json", `{"item":`, 400},
	}
	for _, b := range bodies {
		e.resetReceivers()
		code, _ := e.request(http.MethodPost, "/orders", b.contentType, b.body)
		want := 0
		if b.status == 200 {
			want = 1
		}
		if code != b.status || e.a.count() != want || e.b.count()+e.c.count() != 0 {
			t.Errorf("POST /orders %s: status %d receipts A=%d, want %d and %d", b.name, code, e.a.count(), b.status, want)
		}
	}
	e.resetReceivers()
	if code, _ := e.request(http.MethodGet, "/orders/42", "", "hello"); code != http.StatusBadRequest || e.a.count() != 0 {
		t.Errorf("body on bodyless operation: %d, receipts %d", code, e.a.count())
	}
	if t.Failed() {
		t.Logf("gateway output:\n%s", e.output)
	}
}

func TestForgedHeadersAndMalformedInput(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.policy(e.a, false), seedOptions{})
	e.start(binaries.plain)
	g := e.status().Generation

	code, _ := e.raw("GET /a/x HTTP/1.1\r\nHost: gw\r\n" +
		"X-Gateway-Generation: forged\r\nX-Gateway-Subject: admin\r\nX-Request-Id: forged\r\n" +
		"X-Forwarded-For: 203.0.113.9\r\nX-Forwarded-Host: evil\r\nAuthorization: Bearer secret-token\r\n" +
		"X_Gateway_Generation: forged\r\nX.Gateway.Subject: admin\r\nConnection: close\r\n\r\n")
	if code != http.StatusOK {
		t.Fatalf("forged-header request: %d", code)
	}
	got := e.a.receipts()[0]
	h := got.Headers
	switch {
	case got.Generation != g:
		t.Errorf("generation header %q, want %q", got.Generation, g)
	case h.Get("X-Gateway-Subject") != "":
		t.Error("forged subject header reached the upstream")
	case got.RequestID == "forged" || got.RequestID == "":
		t.Errorf("request id %q not replaced", got.RequestID)
	case h.Get("X-Forwarded-For") != "127.0.0.1":
		t.Errorf("X-Forwarded-For %q, want peer address only", h.Get("X-Forwarded-For"))
	case h.Get("X-Forwarded-Host") == "evil":
		t.Error("forged X-Forwarded-Host reached the upstream")
	case got.Authorization:
		t.Error("client credential reached the upstream")
	}
	// A name with '_' or '.' would read as a gateway header to an upstream
	// that maps header names to CGI-style variables.
	for _, name := range []string{"X_Gateway_Generation", "X.Gateway.Subject"} {
		if h.Get(name) != "" {
			t.Errorf("header %s reached the upstream", name)
		}
	}

	e.resetReceivers()
	requests := []struct {
		name    string
		request string
		status  int
	}{
		{"overlong method", strings.Repeat("G", 33) + " /a/x HTTP/1.1\r\nHost: gw\r\n\r\n", 405},
		{"missing Host", "GET /a/x HTTP/1.1\r\n\r\n", 400},
		{"malformed request line", "GET\r\n\r\n", 400},
		{"declared oversize body", "POST /a/item HTTP/1.1\r\nHost: gw\r\nContent-Type: application/json\r\nContent-Length: 20000\r\n\r\n", 413},
		{"chunked body over limit", "POST /a/item HTTP/1.1\r\nHost: gw\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n" +
			fmt.Sprintf("%x\r\n%s\r\n0\r\n\r\n", 1100, strings.Repeat("a", 1100)), 413},
		{"chunked body on bodyless operation", "GET /a/x HTTP/1.1\r\nHost: gw\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n0\r\n\r\n", 400},
		{"protocol upgrade", "GET /a/x HTTP/1.1\r\nHost: gw\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n", 400},
		{"gateway header named in Connection", "GET /a/x HTTP/1.1\r\nHost: gw\r\nConnection: x-gateway-generation\r\n\r\n", 400},
	}
	for _, r := range requests {
		started := time.Now()
		code, _ := e.raw(r.request)
		t.Logf("%s: status %d after %v", r.name, code, time.Since(started).Round(time.Millisecond))
		if code != r.status {
			t.Errorf("%s: status %d, want %d", r.name, code, r.status)
		}
	}
	e.assertReceipts(0, 0, 0)
}
