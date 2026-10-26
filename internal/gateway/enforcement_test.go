package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests run the enforcement handler under a plain net/http server in
// the test process, with no gateway process. Its forwarder is an in-memory
// stand-in for the reverse proxy that records what it is handed;
// the deployment's real receivers stay connected to nothing, so any request
// reaching them would show a forwarding path outside the handler.

// handedRequest is one admitted request as the forwarder received it.
type handedRequest struct {
	bound  bool // the request carried a trusted binding
	dial   string
	method string
	target string
	host   string
	header http.Header
	body   string
}

// memoryForwarder records every request it is handed and answers with
// respond, or with 200 "forwarded" when respond is nil.
type memoryForwarder struct {
	mu      sync.Mutex
	got     []handedRequest
	respond func(w http.ResponseWriter, r *http.Request) error
}

func (f *memoryForwarder) forward(w http.ResponseWriter, r *http.Request) error {
	b, bound := r.Context().Value(bindingKey{}).(binding)
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.got = append(f.got, handedRequest{
		bound:  bound,
		dial:   b.dial,
		method: r.Method,
		target: r.RequestURI,
		host:   r.Host,
		header: r.Header.Clone(),
		body:   string(body),
	})
	respond := f.respond
	f.mu.Unlock()
	if respond != nil {
		return respond(w, r)
	}
	io.WriteString(w, "forwarded")
	return nil
}

func (f *memoryForwarder) handed() []handedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]handedRequest(nil), f.got...)
}

func (f *memoryForwarder) setRespond(respond func(w http.ResponseWriter, r *http.Request) error) {
	f.mu.Lock()
	f.respond = respond
	f.mu.Unlock()
}

// serveEnforcement seeds e's database with policy, owns its controller and
// serves the enforcement handler, with the global body ceiling maxBodyBytes
// (0: the default), at e.dataAddr, where the harness request helpers send.
func (e *env) serveEnforcement(policy Policy, maxBodyBytes int64) (*Controller, *Handler, *memoryForwarder) {
	e.t.Helper()
	seed(e.t, e.database, policy, seedOptions{})
	c, _ := e.own(e.controllerConfig())
	f := &memoryForwarder{}
	h, err := newHandler(c, maxBodyBytes, f.forward)
	if err != nil {
		e.t.Fatal(err)
	}
	server := httptest.NewServer(h)
	e.t.Cleanup(server.Close)
	e.dataAddr = server.Listener.Addr().String()
	return c, h, f
}

// eventOf returns the last event of the request with the given identifier,
// waiting for want events of it.
func (e *env) eventOf(requestID string, want int) map[string]any {
	e.t.Helper()
	events := e.requestEvents(requestID, want)
	if len(events) != want {
		e.t.Fatalf("request %s has %d events, want %d: %v", requestID, len(events), want, events)
	}
	return events[want-1]
}

// admittedStages is the stage map of a forwarded request to an operation
// without a rate rule, body or header schema.
func admittedStages(forward string) map[string]any {
	stages := expectedStages([]string{"INGRESS", "CAPTURE", "CANONICAL", "ROUTE", "ACCESS", "JWT", "RBAC", "BODY", "AUDIT", "BIND"}, "FORWARD", forward)
	stages["RATE"] = stageNotApplicable
	stages["VALIDATION"] = stageNotApplicable
	return stages
}

// The handler needs a controller and a forwarder; a ceiling of zero selects
// the default.
func TestNewHandlerRefusesIncompleteSetup(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.policy(e.a, false), seedOptions{})
	c, _ := e.own(e.controllerConfig())
	forward := (&memoryForwarder{}).forward
	if h, err := newHandler(nil, 0, forward); h != nil || err == nil {
		t.Fatalf("without a controller: %v, %v", h, err)
	}
	if h, err := newHandler(c, 0, nil); h != nil || err == nil {
		t.Fatalf("without a forwarder: %v, %v", h, err)
	}
	h, err := newHandler(c, 0, forward)
	if err != nil || h.maxBodyBytes != defaultMaxBodyBytes {
		t.Fatalf("default ceiling: %v, %v", h, err)
	}
}

// Under a plain net/http server an admitted request reaches the forwarder
// once, with the trusted binding of its captured generation, the gateway's
// own metadata and no credential, after a durable admission record; no real
// upstream is contacted.
func TestEnforcementHandlerAdmits(t *testing.T) {
	e := newEnv(t)
	c, _, f := e.serveEnforcement(e.protectedPolicy(alice, bob), 0)
	g := c.Current()
	admittedFirst := false
	f.setRespond(func(w http.ResponseWriter, r *http.Request) error {
		// The admission record is durable before the forwarder runs.
		id := r.Header.Get("X-Request-Id")
		for _, ev := range e.auditEvents() {
			admittedFirst = admittedFirst || (ev["kind"] == "request.admitted" && ev["request_id"] == id)
		}
		w.Header().Set("X-Upstream", "memory")
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, "created")
		return nil
	})

	token := issuer.tokenFor(t, "alice")
	resp, body := e.exchange(http.MethodGet, "/a/x?q=1", "", "", "Authorization", "Bearer "+token,
		"X-Request-Id", "forged", "X-Gateway-Subject", "admin", "X-Gateway-Roles", "ADMIN")
	if resp.StatusCode != http.StatusCreated || body != "created" || resp.Header.Get("X-Upstream") != "memory" {
		t.Fatalf("admitted request: %d %q %v", resp.StatusCode, body, resp.Header)
	}
	got := f.handed()
	if len(got) != 1 {
		t.Fatalf("%d requests handed on, want 1", len(got))
	}
	h := got[0]
	switch {
	case !h.bound || h.dial != e.a.addr:
		t.Fatalf("binding %v %q, want %q", h.bound, h.dial, e.a.addr)
	case h.method != http.MethodGet || h.target != "/a/x?q=1":
		t.Fatalf("handed %s %s", h.method, h.target)
	case h.header.Get("X-Gateway-Generation") != g.ID:
		t.Fatalf("generation %q, want %q", h.header.Get("X-Gateway-Generation"), g.ID)
	case h.header.Get("X-Gateway-Subject") != "alice" || h.header.Get("X-Gateway-Roles") != "WRITER":
		t.Fatalf("identity headers %v", h.header)
	case h.header.Get("X-Request-Id") == "" || h.header.Get("X-Request-Id") == "forged":
		t.Fatalf("request identifier %q", h.header.Get("X-Request-Id"))
	case h.header.Get("Authorization") != "":
		t.Fatal("the credential was handed on")
	case !admittedFirst:
		t.Fatal("the admission record was not durable before forwarding")
	}
	ev := e.eventOf(h.header.Get("X-Request-Id"), 2)
	if ev["kind"] != "request.forwarded" || ev["decision"] != "forwarded" || ev["status"] != float64(201) ||
		ev["generation"] != g.ID || ev["upstream"] != "A" || ev["subject"] != "alice" ||
		!sameStages(ev["stages"], admittedStages(stagePass)) {
		t.Fatalf("outcome event %v", ev)
	}
	if audit := e.auditEvents(); strings.Contains(fmt.Sprint(audit), token[:40]) || strings.Contains(fmt.Sprint(audit), "q=1") {
		t.Fatal("the audit log holds the token or the query")
	}
	e.assertReceipts(0, 0, 0)
}

// When the audit log cannot durably record an admission, nothing is
// forwarded: the request is refused at the AUDIT stage, after every other
// stage passed, and the refusal it cannot record is counted as lost.
func TestEnforcementAuditFailureBlocksForwarding(t *testing.T) {
	e := newEnv(t)
	c, _, f := e.serveEnforcement(e.protectedPolicy(alice, bob), 0)
	if code, _ := e.get("/a/x", "Authorization", "Bearer "+issuer.tokenFor(t, "alice")); code != http.StatusOK {
		t.Fatalf("before the failure: %d", code)
	}
	// The state a failed write or sync leaves the log in.
	c.audit.fail(errors.New("injected write failure"))
	lost := c.audit.lost.Load()
	code, body := e.get("/a/x", "Authorization", "Bearer "+issuer.tokenFor(t, "alice"))
	var reply struct {
		Error     string `json:"error"`
		RequestID string `json:"request_id"`
	}
	json.Unmarshal([]byte(body), &reply)
	if code != http.StatusServiceUnavailable || reply.Error != "gateway_unavailable" || reply.RequestID == "" {
		t.Fatalf("with a failed audit log: %d %s", code, body)
	}
	if n := len(f.handed()); n != 1 {
		t.Fatalf("%d requests handed on, want 1", n)
	}
	if got := c.audit.lost.Load(); got != lost+1 {
		t.Fatalf("lost events %d, want %d", got, lost+1)
	}
	// A refusal before the AUDIT stage needs no acknowledgement and is
	// answered as before.
	if code, _ := e.get("/a/x"); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated with a failed audit log: %d", code)
	}
	e.assertReceipts(0, 0, 0)
}

// Every refusal is decided at its stage, answered with its status, error code
// and request identifier, recorded with its reason, and never handed on.
func TestEnforcementRefusals(t *testing.T) {
	e := newEnv(t)
	p := e.protectedPolicy(alice, bob, Subject{ID: "carol", Status: "disabled", Roles: []string{"WRITER"}})
	p.Operations = append(p.Operations,
		Operation{ID: "strict", API: "a", Method: "POST", Target: "/a/strict", Access: "allow", Auth: "required",
			Body: jsonBody(1024, `{"type":"object","additionalProperties":true,"required":["n"],"properties":{"n":{"type":"integer"}}}`)},
		Operation{ID: "probe", API: "a", Method: "GET", Target: "/a/probe", Access: "allow", Auth: "none",
			Headers: json.RawMessage(`{"type":"object","additionalProperties":true,"required":["X-Probe"],"properties":{"X-Probe":{"type":"string","enum":["on"]}}}`)},
	)
	p.Permits = append(p.Permits, Permit{Role: "WRITER", Operation: "strict"})
	_, _, f := e.serveEnforcement(p, 0)

	bearer := func(subject string) string { return "Bearer " + issuer.tokenFor(t, subject) }
	claims := func(changes map[string]any) string {
		return "Bearer " + issuer.token(t, changed(validClaims("alice"), changes))
	}
	valid := issuer.tokenFor(t, "alice")
	segments := strings.Split(valid, ".")
	tampered := segments[0] + "." + segments[1] + ".AAAA" + segments[2][4:]
	raw := func(request string) string { return request + "Host: gw\r\nConnection: close\r\n\r\n" }

	cases := []struct {
		name    string
		raw     string   // sent byte for byte when set
		method  string   // otherwise the request
		path    string   //
		ctype   string   //
		body    string   //
		header  []string // name/value pairs
		status  int
		code    string
		stage   string
		reason  string
		allowed string // expected Allow header
	}{
		{name: "absolute-form target", raw: raw("GET http://" + e.a.addr + "/a/x HTTP/1.1\r\n"), status: 400, code: "bad_request", stage: "INGRESS"},
		{name: "tunnel", raw: raw("CONNECT " + e.a.addr + " HTTP/1.1\r\n"), status: 400, code: "bad_request", stage: "INGRESS"},
		{name: "protocol upgrade", raw: raw("GET /a/x HTTP/1.1\r\nUpgrade: websocket\r\n"), status: 400, code: "bad_request", stage: "INGRESS"},
		{name: "Connection naming a gateway header", raw: raw("GET /a/x HTTP/1.1\r\nConnection: X-Gateway-Subject\r\n"), status: 400, code: "bad_request", stage: "INGRESS"},
		{name: "declared length above the global ceiling", raw: raw("POST /a/item HTTP/1.1\r\nContent-Type: application/json\r\nContent-Length: 16385\r\n"),
			status: 413, code: "payload_too_large", stage: "INGRESS"},
		{name: "encoded dot segment", raw: raw("GET /a/%2e%2e/x HTTP/1.1\r\n"), status: 400, code: "bad_request", stage: "CANONICAL"},
		{name: "empty segment", raw: raw("GET /a//x HTTP/1.1\r\n"), status: 400, code: "bad_request", stage: "CANONICAL"},
		{name: "unknown target", method: "GET", path: "/zzz", status: 404, code: "not_found", stage: "ROUTE"},
		{name: "method not declared", method: "DELETE", path: "/a/x", status: 405, code: "method_not_allowed", stage: "ROUTE", allowed: "GET"},
		{name: "denied operation", method: "GET", path: "/d", status: 403, code: "forbidden", stage: "ACCESS"},
		{name: "no token", method: "GET", path: "/a/x", status: 401, code: "unauthenticated", stage: "JWT", reason: "missing_credential"},
		{name: "expired token", method: "GET", path: "/a/x", header: []string{"Authorization", claims(map[string]any{"exp": time.Now().Add(-time.Minute).Unix()})},
			status: 401, code: "unauthenticated", stage: "JWT", reason: "expired"},
		{name: "wrong audience", method: "GET", path: "/a/x", header: []string{"Authorization", claims(map[string]any{"aud": "other"})},
			status: 401, code: "unauthenticated", stage: "JWT", reason: "wrong_audience"},
		{name: "signature altered", method: "GET", path: "/a/x", header: []string{"Authorization", "Bearer " + tampered},
			status: 401, code: "unauthenticated", stage: "JWT", reason: "invalid_signature"},
		{name: "unsigned token", method: "GET", path: "/a/x", header: []string{"Authorization", "Bearer " + encodeSegment(`{"alg":"none"}`) + "." + segments[1] + "."},
			status: 401, code: "unauthenticated", stage: "JWT", reason: "malformed_credential"},
		{name: "unknown subject", method: "GET", path: "/a/x", header: []string{"Authorization", bearer("mallory")},
			status: 403, code: "forbidden", stage: "RBAC", reason: "unknown_subject"},
		{name: "subject without a permit", method: "POST", path: "/a/item", ctype: "application/json", body: `{"n":1}`, header: []string{"Authorization", bearer("bob")},
			status: 403, code: "forbidden", stage: "RBAC", reason: "no_permit"},
		{name: "role claimed in the token", method: "POST", path: "/a/item", ctype: "application/json", body: `{"n":1}`,
			header: []string{"Authorization", "Bearer " + issuer.token(t, changed(validClaims("bob"), map[string]any{"roles": []string{"WRITER"}}))},
			status: 403, code: "forbidden", stage: "RBAC", reason: "no_permit"},
		{name: "disabled subject", method: "GET", path: "/a/x", header: []string{"Authorization", bearer("carol")},
			status: 403, code: "forbidden", stage: "RBAC", reason: "disabled_subject"},
		{name: "body where none is declared", method: "GET", path: "/a/x", body: "x", header: []string{"Authorization", bearer("alice")},
			status: 400, code: "bad_request", stage: "BODY", reason: "unexpected_body"},
		{name: "media type", method: "POST", path: "/a/item", ctype: "text/plain", body: `{"n":1}`, header: []string{"Authorization", bearer("alice")},
			status: 415, code: "unsupported_media_type", stage: "BODY", reason: "media_type"},
		{name: "malformed JSON", method: "POST", path: "/a/item", ctype: "application/json", body: `{"n":`, header: []string{"Authorization", bearer("alice")},
			status: 400, code: "bad_request", stage: "BODY", reason: "malformed_json"},
		{name: "body schema", method: "POST", path: "/a/strict", ctype: "application/json", body: `{"n":"one"}`, header: []string{"Authorization", bearer("alice")},
			status: 400, code: "bad_request", stage: "VALIDATION", reason: "schema_mismatch"},
		{name: "required header missing", method: "GET", path: "/a/probe", status: 400, code: "bad_request", stage: "VALIDATION", reason: "header_mismatch"},
		{name: "header value outside its schema", method: "GET", path: "/a/probe", header: []string{"X-Probe", "off"},
			status: 400, code: "bad_request", stage: "VALIDATION", reason: "header_mismatch"},
		{name: "repeated header", raw: raw("GET /a/probe HTTP/1.1\r\nX-Probe: on\r\nX-Probe: on\r\n"),
			status: 400, code: "bad_request", stage: "VALIDATION", reason: "repeated_header"},
	}
	for _, tc := range cases {
		var resp *http.Response
		var body string
		if tc.raw != "" {
			resp, body = e.rawResponse(tc.raw)
		} else {
			resp, body = e.exchange(tc.method, tc.path, tc.ctype, tc.body, tc.header...)
		}
		var reply struct {
			Error     string `json:"error"`
			RequestID string `json:"request_id"`
		}
		json.Unmarshal([]byte(body), &reply)
		if resp.StatusCode != tc.status || reply.Error != tc.code || reply.RequestID == "" {
			t.Errorf("%s: %d %s, want %d %s", tc.name, resp.StatusCode, body, tc.status, tc.code)
			continue
		}
		if tc.allowed != "" && !strings.Contains(resp.Header.Get("Allow"), tc.allowed) {
			t.Errorf("%s: Allow %q", tc.name, resp.Header.Get("Allow"))
		}
		if tc.stage == "JWT" && resp.Header.Get("WWW-Authenticate") != "Bearer" {
			t.Errorf("%s: WWW-Authenticate %q", tc.name, resp.Header.Get("WWW-Authenticate"))
		}
		ev := e.eventOf(reply.RequestID, 1)
		reason, _ := ev["reason"].(string)
		if ev["kind"] != "request.rejected" || ev["stage"] != tc.stage || ev["error"] != tc.code ||
			ev["status"] != float64(tc.status) || ev["decision"] != "reject" || reason != tc.reason {
			t.Errorf("%s: event %v, want %s at %s (%q)", tc.name, ev, tc.code, tc.stage, tc.reason)
		}
		if n := len(f.handed()); n != 0 {
			t.Fatalf("%s: %d requests handed on", tc.name, n)
		}
	}

	// The same operations admit valid requests.
	if code, _ := e.request(http.MethodPost, "/a/strict", "application/json", `{"n":1}`, "Authorization", bearer("alice")); code != http.StatusOK {
		t.Fatalf("valid body: %d", code)
	}
	if code, _ := e.get("/a/probe", "X-Probe", "on"); code != http.StatusOK {
		t.Fatalf("valid header: %d", code)
	}
	got := f.handed()
	if len(got) != 2 || got[0].body != `{"n":1}` || got[1].header.Get("X-Probe") != "on" {
		t.Fatalf("handed %+v", got)
	}
	e.assertReceipts(0, 0, 0)
}

// The effective body ceiling, the smaller of the global and the operation's,
// bounds the bytes read for a declared and for a chunked body: exactly the
// ceiling is handed on whole with its length declared, one byte more is
// refused with 413 and never handed on.
func TestEnforcementBodyCeiling(t *testing.T) {
	e := newEnv(t)
	p := e.policy(e.a, false)
	p.Operations = append(p.Operations,
		Operation{ID: "large", API: "a", Method: "POST", Target: "/a/large", Access: "allow", Auth: "none", Body: jsonBody(4096, anyObject)},
		Operation{ID: "small", API: "a", Method: "POST", Target: "/a/small", Access: "allow", Auth: "none", Body: jsonBody(1024, anyObject)},
		Operation{ID: "equal", API: "a", Method: "POST", Target: "/a/equal", Access: "allow", Auth: "none", Body: jsonBody(2048, anyObject)},
	)
	const global = 2048
	_, _, f := e.serveEnforcement(p, global)
	routed := []string{"INGRESS", "CAPTURE", "CANONICAL", "ROUTE", "ACCESS"}
	operations := []struct {
		name, path string
		ceiling    int
	}{
		{"global below the operation", "/a/large", global},
		{"operation below the global", "/a/small", 1024},
		{"equal ceilings", "/a/equal", global},
	}
	handed := 0
	for _, op := range operations {
		for _, framing := range []string{"declared", "chunked"} {
			for _, size := range []int{op.ceiling, op.ceiling + 1} {
				name := fmt.Sprintf("%s, %s, %d bytes", op.name, framing, size)
				body := sizedBody("", size)
				status, reply := e.sendBody(framing, op.path, body)
				if size == op.ceiling {
					handed++
					got := f.handed()
					if status != http.StatusOK || len(got) != handed {
						t.Fatalf("%s: status %d (%s), %d handed on, want 200 and %d", name, status, reply, len(got), handed)
					}
					last := got[handed-1]
					if last.body != body || last.header.Get("Content-Length") != fmt.Sprint(size) || last.header.Get("Transfer-Encoding") != "" {
						t.Errorf("%s: handed %d bytes (intact %v) with Content-Length %q", name, len(last.body), last.body == body, last.header.Get("Content-Length"))
					}
					continue
				}
				stage, reason := sizeRefusal(framing, size, global)
				want := expectedStages(nil, "INGRESS", stageReject)
				if stage == "BODY" {
					want = public(expectedStages(routed, "BODY", stageReject))
				}
				_, id := requestID(&http.Response{StatusCode: status}, reply)
				ev := e.eventOf(id, 1)
				if got, _ := ev["reason"].(string); status != http.StatusRequestEntityTooLarge || ev["stage"] != stage || got != reason ||
					ev["error"] != "payload_too_large" || !sameStages(ev["stages"], want) {
					t.Errorf("%s: status %d (%s), event %v, want 413 at %s", name, status, reply, ev, stage)
				}
				if n := len(f.handed()); n != handed {
					t.Fatalf("%s: %d handed on, want %d: a refused body was forwarded", name, n, handed)
				}
			}
		}
	}
	e.assertReceipts(0, 0, 0)
}

// Rate limits count admitted requests per verified subject; a refusal is
// answered at the RATE stage and not handed on, and a publication keeps the
// counters, so a new generation forgives nothing.
func TestEnforcementRateLimit(t *testing.T) {
	e := newEnv(t)
	p := e.protectedPolicy(alice, bob)
	p.Operations[0].Rate = rate(2, 60)
	c, _, f := e.serveEnforcement(p, 0)
	get := func(subject string) (int, string) {
		resp, body := e.exchange(http.MethodGet, "/a/x", "", "", "Authorization", "Bearer "+issuer.tokenFor(t, subject))
		_, id := requestID(resp, body)
		return resp.StatusCode, id
	}
	for i := range 2 {
		if code, _ := get("alice"); code != http.StatusOK {
			t.Fatalf("alice request %d: %d", i+1, code)
		}
	}
	code, id := get("alice")
	if code != http.StatusTooManyRequests {
		t.Fatalf("third alice request: %d", code)
	}
	if ev := e.eventOf(id, 1); ev["stage"] != "RATE" || ev["error"] != "rate_limited" || ev["reason"] != "quota_exceeded" {
		t.Fatalf("refusal event %v", ev)
	}
	if code, _ := get("bob"); code != http.StatusOK {
		t.Fatalf("bob has his own counter: %d", code)
	}

	next := p
	next.Subjects = append([]Subject{}, alice, bob, Subject{ID: "dave", Status: "active", Roles: []string{"READER"}})
	if outcome, status := c.Publish(context.Background(), "test", Change{ID: "rate-continuity", Expected: 1, Policy: mustJSON(next)}); status != http.StatusOK {
		t.Fatalf("publication: %d %+v", status, outcome)
	}
	if code, _ := get("alice"); code != http.StatusTooManyRequests {
		t.Fatalf("alice after the publication: %d, want 429", code)
	}
	if n := len(f.handed()); n != 3 {
		t.Fatalf("%d requests handed on, want 3", n)
	}
	e.assertReceipts(0, 0, 0)
}

// publishOnRead is a request body that publishes a new generation when the
// handler first reads it, between the capture and the later stages.
type publishOnRead struct {
	data    *strings.Reader
	publish func()
	once    sync.Once
}

func (b *publishOnRead) Read(p []byte) (int, error) {
	b.once.Do(b.publish)
	return b.data.Read(p)
}

func (b *publishOnRead) Close() error { return nil }

// A request is decided by the one generation it captured, even when another
// is published while it is in progress: the body schema, the binding and the
// metadata all come from it. The next request uses the new generation.
func TestEnforcementUsesCapturedGeneration(t *testing.T) {
	e := newEnv(t)
	c, h, f := e.serveEnforcement(e.policy(e.a, false), 0)
	captured := c.Current()

	// The next generation sends API a to B and requires the member "x".
	next := e.policy(e.b, false)
	next.Operations[1].Body = jsonBody(1024, `{"type":"object","additionalProperties":true,"required":["x"],"properties":{"x":{"type":"integer"}}}`)
	var published Outcome
	body := &publishOnRead{data: strings.NewReader(`{"n":1}`), publish: func() {
		var status int
		published, status = c.Publish(context.Background(), "test", Change{ID: "during-request", Expected: 1, Policy: mustJSON(next)})
		if status != http.StatusOK {
			t.Errorf("publication during the request: %d %+v", status, published)
		}
	}}
	req := httptest.NewRequest(http.MethodPost, "/a/item", body)
	req.ContentLength = int64(body.data.Len())
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || published.Revision != 2 || c.Current().ID != published.Generation {
		t.Fatalf("request during publication: %d %s; published %+v", rec.Code, rec.Body, published)
	}
	got := f.handed()
	if len(got) != 1 || got[0].dial != e.a.addr || got[0].header.Get("X-Gateway-Generation") != captured.ID {
		t.Fatalf("handed %+v, want upstream A under %s", got, captured.ID)
	}
	if ev := e.eventOf(got[0].header.Get("X-Request-Id"), 2); ev["generation"] != captured.ID || ev["upstream"] != "A" {
		t.Fatalf("outcome event %v", ev)
	}

	if code, body := e.request(http.MethodPost, "/a/item", "application/json", `{"n":1}`); code != http.StatusBadRequest {
		t.Fatalf("body refused by the new schema: %d %s", code, body)
	}
	if code, _ := e.request(http.MethodPost, "/a/item", "application/json", `{"x":1}`); code != http.StatusOK {
		t.Fatalf("body valid under the new schema: %d", code)
	}
	got = f.handed()
	if len(got) != 2 || got[1].dial != e.b.addr || got[1].header.Get("X-Gateway-Generation") != published.Generation {
		t.Fatalf("handed %+v, want upstream B under %s", got[1:], published.Generation)
	}
	e.assertReceipts(0, 0, 0)
}

// Without a usable generation nothing is admitted: none published, one past
// its deadline as a late timer would leave it (the controller then withdraws
// it rather than serve it), and a stopped controller.
func TestEnforcementWithoutUsableGeneration(t *testing.T) {
	t.Run("none published", func(t *testing.T) {
		e := newEnv(t)
		c, _ := e.own(e.controllerConfig())
		f := &memoryForwarder{}
		h, err := newHandler(c, 0, f.forward)
		if err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/a/x", nil))
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "gateway_unavailable") || len(f.handed()) != 0 {
			t.Fatalf("without authority: %d %s", rec.Code, rec.Body)
		}
	})

	t.Run("past its deadline", func(t *testing.T) {
		e := newEnv(t)
		policy := e.policy(e.a, false)
		c, _, f := e.serveEnforcement(policy, 0)
		stale, err := effectiveGeneration(policy, 1, time.Now().Add(-time.Second), nil)
		if err != nil {
			t.Fatal(err)
		}
		c.current.Store(stale)
		resp, body := e.exchange(http.MethodGet, "/a/x", "", "", "")
		_, id := requestID(resp, body)
		if resp.StatusCode != http.StatusServiceUnavailable || len(f.handed()) != 0 {
			t.Fatalf("expired generation: %d %s", resp.StatusCode, body)
		}
		if ev := e.eventOf(id, 1); ev["stage"] != "CAPTURE" || ev["error"] != "gateway_unavailable" {
			t.Fatalf("refusal event %v", ev)
		}
		// No stored deadline lies behind it, so the controller stops serving
		// rather than trust it.
		waitFor(t, time.Now().Add(5*time.Second), "the controller to withdraw the generation", func() bool { return c.Current() == nil })
		if reason := e.unavailableReason(); !strings.Contains(reason, "expired but no stored deadline has passed") {
			t.Fatalf("reason %q", reason)
		}
		e.assertReceipts(0, 0, 0)
	})

	t.Run("controller stopped", func(t *testing.T) {
		e := newEnv(t)
		c, _, f := e.serveEnforcement(e.policy(e.a, false), 0)
		if code, _ := e.get("/a/x"); code != http.StatusOK {
			t.Fatalf("before Stop: %d", code)
		}
		if err := c.Stop(); err != nil {
			t.Fatal(err)
		}
		if code, body := e.get("/a/x"); code != http.StatusServiceUnavailable || !strings.Contains(body, "gateway_unavailable") {
			t.Fatalf("after Stop: %d %s", code, body)
		}
		if n := len(f.handed()); n != 1 {
			t.Fatalf("%d requests handed on, want 1", n)
		}
	})
}

// Concurrent requests during repeated publications: every request handed on
// carries the generation it was admitted under, its binding is that
// generation's upstream, and its outcome event names the same generation.
func TestEnforcementGenerationsUnderConcurrency(t *testing.T) {
	e := newEnv(t)
	toA, toB := e.policy(e.a, false), e.policy(e.b, false)
	c, _, f := e.serveEnforcement(toA, 0)
	var dials sync.Map // generation ID -> the dial of API a in it
	dials.Store(c.Current().ID, e.a.addr)

	done := make(chan struct{})
	published := 0
	var wg sync.WaitGroup
	wg.Go(func() {
		expected := int64(1)
		for i := 0; ; i++ {
			select {
			case <-done:
				return
			default:
			}
			policy, dial := toB, e.b.addr
			if i%2 == 1 {
				policy, dial = toA, e.a.addr
			}
			outcome, status := c.Publish(context.Background(), "test", Change{ID: fmt.Sprintf("concurrent-%d", i), Expected: expected, Policy: mustJSON(policy)})
			if status != http.StatusOK {
				t.Errorf("publication %d: %d %+v", i, status, outcome)
				return
			}
			dials.Store(outcome.Generation, dial)
			expected = outcome.Revision
			published++
		}
	})
	var clients sync.WaitGroup
	for range 8 {
		clients.Go(func() {
			for range 50 {
				if code, body := e.get("/a/x"); code != http.StatusOK {
					t.Errorf("request: %d %s", code, body)
				}
			}
		})
	}
	clients.Wait()
	close(done)
	wg.Wait()

	got := f.handed()
	if len(got) != 400 {
		t.Fatalf("%d requests handed on, want 400", len(got))
	}
	outcomes := map[string]string{} // request ID -> generation of its outcome event
	for _, ev := range e.eventsWhen(func(events []map[string]any) bool { return countKind(events, "request.forwarded") == 400 }) {
		if ev["kind"] == "request.forwarded" {
			outcomes[ev["request_id"].(string)], _ = ev["generation"].(string)
		}
	}
	seen := map[string]bool{}
	for _, h := range got {
		generation := h.header.Get("X-Gateway-Generation")
		dial, known := dials.Load(generation)
		if !known || h.dial != dial || outcomes[h.header.Get("X-Request-Id")] != generation {
			t.Fatalf("request under %s bound to %s (want %v), outcome event under %s",
				generation, h.dial, dial, outcomes[h.header.Get("X-Request-Id")])
		}
		seen[generation] = true
	}
	t.Logf("%d publications, %d generations seen by requests", published, len(seen))
	e.assertReceipts(0, 0, 0)
}

// The forwarder's response and failures reach the client and the outcome
// event truthfully: a failure before any response is answered by the gateway
// with its mapped status, an aborted response stays aborted, and a cancelled
// request is not recorded as forwarded.
func TestEnforcementResponses(t *testing.T) {
	e := newEnv(t)
	_, _, f := e.serveEnforcement(e.policy(e.a, false), 0)
	forwardedEvent := func(id string) map[string]any { return e.eventOf(id, 2) }

	t.Run("informational then final", func(t *testing.T) {
		f.setRespond(func(w http.ResponseWriter, r *http.Request) error {
			w.Header().Set("Link", "</style.css>; rel=preload")
			w.WriteHeader(http.StatusEarlyHints)
			w.WriteHeader(http.StatusAccepted)
			io.WriteString(w, "accepted")
			return nil
		})
		resp, body := e.exchange(http.MethodGet, "/a/x", "", "", "")
		id := f.handed()[len(f.handed())-1].header.Get("X-Request-Id")
		if ev := forwardedEvent(id); resp.StatusCode != http.StatusAccepted || body != "accepted" || ev["status"] != float64(202) || ev["decision"] != "forwarded" {
			t.Fatalf("%d %q, event %v", resp.StatusCode, body, ev)
		}
	})

	failures := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"upstream timed out", fmt.Errorf("waiting for headers: %w", errUpstreamTimeout), http.StatusGatewayTimeout, "upstream_timeout"},
		{"no upstream", fmt.Errorf("source failed: %w", errNoUpstream), http.StatusServiceUnavailable, "gateway_unavailable"},
		{"upstream unreachable", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}, http.StatusBadGateway, "upstream_unavailable"},
	}
	for _, tc := range failures {
		t.Run(tc.name, func(t *testing.T) {
			f.setRespond(func(w http.ResponseWriter, r *http.Request) error { return tc.err })
			resp, body := e.exchange(http.MethodGet, "/a/x", "", "", "")
			_, id := requestID(resp, body)
			ev := forwardedEvent(id)
			if resp.StatusCode != tc.status || !strings.Contains(body, `"error":"`+tc.code+`"`) || ev["decision"] != "upstream_failure" ||
				ev["error"] != tc.code || ev["status"] != float64(tc.status) || !sameStages(ev["stages"], publicAdmitted(stageFail)) {
				t.Fatalf("%d %s, event %v", resp.StatusCode, body, ev)
			}
			if strings.Contains(body, "connection refused") || strings.Contains(body, "source failed") {
				t.Fatalf("the reply discloses the failure: %s", body)
			}
		})
	}

	t.Run("aborted after the status", func(t *testing.T) {
		f.setRespond(func(w http.ResponseWriter, r *http.Request) error {
			w.Header().Set("Content-Length", "100")
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, "partial")
			http.NewResponseController(w).Flush()
			panic(http.ErrAbortHandler)
		})
		conn, err := net.Dial("tcp", e.dataAddr)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		io.WriteString(conn, "GET /a/x HTTP/1.1\r\nHost: gw\r\n\r\n")
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK || string(data) != "partial" || !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("aborted response: %d %q %v", resp.StatusCode, data, err)
		}
		id := f.handed()[len(f.handed())-1].header.Get("X-Request-Id")
		if ev := forwardedEvent(id); ev["decision"] != "upstream_failure" || ev["error"] != "upstream_incomplete" || ev["status"] != float64(200) {
			t.Fatalf("event %v", ev)
		}
	})

	t.Run("client cancels", func(t *testing.T) {
		entered := make(chan struct{})
		f.setRespond(func(w http.ResponseWriter, r *http.Request) error {
			close(entered)
			<-r.Context().Done()
			return r.Context().Err()
		})
		ctx, cancel := context.WithCancel(context.Background())
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+e.dataAddr+"/a/x", nil)
		result := make(chan error, 1)
		go func() {
			resp, err := http.DefaultClient.Do(req)
			if err == nil {
				resp.Body.Close()
			}
			result <- err
		}()
		<-entered
		cancel()
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatalf("client: %v", err)
		}
		id := f.handed()[len(f.handed())-1].header.Get("X-Request-Id")
		if ev := forwardedEvent(id); ev["decision"] != "upstream_failure" || ev["error"] != "upstream_unavailable" {
			t.Fatalf("event %v", ev)
		}
	})
	e.assertReceipts(0, 0, 0)
}

// publicAdmitted is admittedStages for a public operation.
func publicAdmitted(forward string) map[string]any {
	stages := admittedStages(forward)
	stages["JWT"] = stageNotApplicable
	stages["RBAC"] = stageNotApplicable
	return stages
}

// The trusted binding comes only from the captured generation: no client
// field can choose or change the destination. The forwarder sends a request
// without a binding nowhere (TestNativeForwardingBinding).
func TestEnforcementTrustedBinding(t *testing.T) {
	e := newEnv(t)
	_, _, f := e.serveEnforcement(e.policy(e.a, true), 0)
	// Every field a client could use to name a destination names B.
	forged := "Host: " + e.b.addr + "\r\nX-Forwarded-Host: " + e.b.addr + "\r\nForwarded: host=" + e.b.addr +
		"\r\nX-Gateway-Upstream: B\r\nX-Gateway-Dial: " + e.b.addr + "\r\nConnection: close\r\n\r\n"
	if code, _ := e.raw("GET /a/x HTTP/1.1\r\n" + forged); code != http.StatusOK {
		t.Fatalf("request with forged destination fields: %d", code)
	}
	if code, _ := e.raw("GET /b/x HTTP/1.1\r\nHost: " + e.a.addr + "\r\nConnection: close\r\n\r\n"); code != http.StatusOK {
		t.Fatalf("request to /b: %d", code)
	}
	got := f.handed()
	if len(got) != 2 || !got[0].bound || got[0].dial != e.a.addr || !got[1].bound || got[1].dial != e.b.addr {
		t.Fatalf("bindings %+v, want A then B", got)
	}
	// The inbound Host is kept for the upstream, as before, but decides nothing.
	if got[0].host != e.b.addr || got[0].header.Get("X-Gateway-Upstream") != "" || got[0].header.Get("X-Gateway-Dial") != "" {
		t.Fatalf("handed host %q, headers %v", got[0].host, got[0].header)
	}
	e.assertReceipts(0, 0, 0)
}
