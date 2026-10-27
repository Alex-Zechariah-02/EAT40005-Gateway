package gateway

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests run the gateway, cmd/gateway, as its own process with its
// configuration file. Every forwarding claim is checked at the receivers or
// fixtures themselves.

// The native process serves the three fixture APIs through the one
// enforcement pipeline and the native proxy: every catalogue step reaches
// exactly its fixture or none. Its startup output tells listening apart from
// protected admission.
func TestNativeGatewayServesFixtures(t *testing.T) {
	fixtures := startFixtures(t)
	e := newEnv(t)
	seed(t, e.database, fixturePolicy(fixtures), seedOptions{})
	e.start(binaries.plain)

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

	s := e.status()
	if s.Admission != "ready" || s.State != modeNormal {
		t.Fatalf("status %+v, want normal and ready", s)
	}
	output := e.output.String()
	for _, want := range []string{
		`"msg":"listening","data":"` + e.dataAddr + `","admin":"` + e.admin + `"`,
		`"msg":"protected admission","admission":"ready","generation":"` + s.Generation + `"`,
	} {
		if !strings.Contains(output, want) {
			t.Errorf("startup output lacks %s:\n%s", want, output)
		}
	}
	if strings.Contains(strings.ToLower(output), "caddy") {
		t.Errorf("the native process mentions Caddy:\n%s", output)
	}
}

// The native process listens on exactly its data, admin and metrics addresses. It has
// no Caddy administration endpoint, the data plane reaches no Admin API
// route, and the Admin API keeps its Host, session, Origin, CSRF and
// permission checks and its authority over the served policy, including the
// confirmed-activation and temporary-override lifecycles.
func TestNativeListenersAndAdministration(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.plain, "CADDY_ADMIN="+freeAddr(t))
	before := e.status()

	ports := listeningPorts(t, e.process.Process.Pid)
	if len(ports) != 3 || !strings.Contains(" "+strings.Join(ports, " ")+" ", " "+port(e.dataAddr)+" ") ||
		!strings.Contains(" "+strings.Join(ports, " ")+" ", " "+port(e.admin)+" ") || !strings.Contains(" "+strings.Join(ports, " ")+" ", " "+port(e.metrics)+" ") {
		t.Fatalf("listening ports %v, want exactly data %s, admin %s and metrics %s", ports, port(e.dataAddr), port(e.admin), port(e.metrics))
	}
	if dialable("127.0.0.1:2019") {
		t.Fatal("Caddy's default admin address accepts connections")
	}
	for _, path := range nativeAdminPaths {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			if code, _ := e.request(method, path, "application/json", `{}`); code != http.StatusNotFound {
				t.Errorf("%s %s on the data plane: %d", method, path, code)
			}
			if code, _ := e.adminRaw(method, path, `{}`); code != http.StatusNotFound {
				t.Errorf("%s %s on the admin listener with a session: %d", method, path, code)
			}
		}
	}

	// No Admin API route exists on the data plane, with or without the
	// administrator's session and the admin origin.
	harness := e.mustLogin(harnessAdmin, harnessPassword)
	session := harness.headers(e)
	for _, probe := range []struct{ method, path string }{
		{http.MethodGet, "/admin/health"}, {http.MethodGet, "/admin/status"}, {http.MethodPost, "/admin/session"},
		{http.MethodPut, "/admin/blocked-accounts/mallory"}, {http.MethodPost, "/admin/candidates"}, {http.MethodGet, "/"},
	} {
		resp, body := e.exchange(probe.method, probe.path, "application/json", `{}`, session...)
		_, id := requestID(resp, body)
		if events := e.requestEvents(id, 1); resp.StatusCode != http.StatusNotFound || len(events) != 1 || events[0]["stage"] != "ROUTE" {
			t.Errorf("%s %s on the data plane: %d %s, events %v", probe.method, probe.path, resp.StatusCode, body, events)
		}
	}

	// The Admin API's own boundary.
	membership := `{"reason":"test containment"}`
	put := func(header ...string) int {
		code, _, _ := e.adminSend(http.MethodPut, "/admin/blocked-accounts/alice", membership,
			append([]string{"If-Match", `"r1"`, "Idempotency-Key", "native-admin-" + newToken()[:8]}, header...)...)
		return code
	}
	cookie := []string{"Cookie", sessionCookie + "=" + harness.cookie}
	for name, tc := range map[string]struct {
		header []string
		want   int
	}{
		"no session":      {[]string{"Origin", e.adminOrigin()}, http.StatusUnauthorized},
		"no CSRF token":   {append(cookie, "Origin", e.adminOrigin()), http.StatusForbidden},
		"foreign origin":  {append(cookie, csrfHeader, harness.csrf, "Origin", "http://"+e.dataAddr), http.StatusForbidden},
		"data-plane host": {append(session, "Host", e.dataAddr), http.StatusBadRequest},
	} {
		if got := put(tc.header...); got != tc.want {
			t.Errorf("membership change with %s: %d, want %d", name, got, tc.want)
		}
	}
	if after := e.status(); after.Generation != before.Generation {
		t.Fatalf("refused requests changed the generation to %s", after.Generation)
	}
	if code, _ := e.get("/a/x", bearer(t, "alice")...); code != http.StatusOK {
		t.Fatalf("alice before containment: %d", code)
	}
	if got := put(session...); got != http.StatusOK {
		t.Fatalf("authorised membership change: %d", got)
	}
	member := e.status()
	if member.Revision != 1 || member.Generation != before.Generation {
		t.Fatalf("containment changed policy: %+v", member)
	}
	e.resetReceivers()
	if code, _ := e.get("/a/x", bearer(t, "alice")...); code != http.StatusForbidden {
		t.Fatalf("alice after containment: %d", code)
	}
	if code, body := e.unblockAccount(harness, "alice", "native-account-unblock"); code != 200 {
		t.Fatalf("unblock: %d %s", code, body)
	}
	if code, _ := e.get("/a/x", bearer(t, "alice")...); code != 200 {
		t.Fatalf("unblocked account: %d", code)
	}
	if r := e.a.receipts(); len(r) != 1 || r[0].Generation != member.Generation {
		t.Fatalf("receipts %+v, want one under %s", r, member.Generation)
	}

	// Confirmed activation: the candidate moves API a to C, and its
	// confirmation keeps it there.
	_, outcome := e.provisionalFrom(harness, e.cpaCandidate(), 1, 60, "native-cpa")
	if code, _, body := e.resolve(harness, "confirm", outcome.Provisional, `"r1"`, "native-cpa-confirm"); code != http.StatusOK {
		t.Fatalf("confirmation: %d %s", code, body)
	}
	e.resetReceivers()
	if code, body := e.get("/a/x", bearer(t, "alice")...); code != http.StatusOK || body != "C ok" {
		t.Fatalf("alice after the confirmed activation: %d %q", code, body)
	}

	// A temporary containment override blocks the operation until it is
	// revoked.
	confirmed := e.status().Confirmed
	created := e.mustOverride(harness, operationContainment("read-a", 60), policyETag(confirmed), "native-tpo-create")
	if code, _ := e.get("/a/x", bearer(t, "alice")...); code != http.StatusForbidden {
		t.Fatalf("alice under the containment override: %d", code)
	}
	if code, _, body := e.revokeOverride(harness, created.Override, "native-tpo-revoke"); code != http.StatusOK {
		t.Fatalf("revocation: %d %s", code, body)
	}
	if code, _ := e.get("/a/x", bearer(t, "alice")...); code != http.StatusOK {
		t.Fatalf("alice after the revocation: %d", code)
	}
	e.assertReceipts(0, 0, 2)
}

// The data plane serves HTTP/1.0 and HTTP/1.1 in plain text, nothing else,
// with its configured header, body and timeout limits. Every request it
// refuses itself reaches no upstream.
func TestNativeDataPlaneProtocol(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.policy(e.a, false), seedOptions{})
	settings := e.nativeSettings()
	data := section(settings, "data")
	data["max_body_bytes"] = 512
	data["max_header_bytes"] = 8192
	data["read_header_timeout"] = "1s"
	data["read_timeout"] = "2s"
	data["idle_timeout"] = "1s"
	e.writeNative(settings)
	e.start(binaries.plain)

	resp, body := e.rawResponse("GET /a/x HTTP/1.1\r\nHost: api.example:8080\r\nX-Forwarded-For: 203.0.113.9\r\nConnection: close\r\n\r\n")
	if resp.StatusCode != http.StatusOK || body != "A ok" || resp.Proto != "HTTP/1.1" {
		t.Fatalf("HTTP/1.1 request: %s %d %q", resp.Proto, resp.StatusCode, body)
	}
	if r := e.a.receipts()[0]; r.Host != "api.example:8080" || r.Headers.Get("X-Forwarded-For") != "127.0.0.1" || r.Headers.Get("Via") != "1.1 gateway" {
		t.Fatalf("forwarded Host %q, X-Forwarded-For %q, Via %q", r.Host, r.Headers.Get("X-Forwarded-For"), r.Headers.Get("Via"))
	}
	if code, body := e.raw("GET /a/x HTTP/1.0\r\nHost: gw\r\n\r\n"); code != http.StatusOK || body != "A ok" {
		t.Fatalf("HTTP/1.0 request: %d %q", code, body)
	}
	e.assertReceipts(2, 0, 0)

	// Cleartext HTTP/2 with prior knowledge is not served.
	if resp, err := newPriorKnowledgeClient(t).Get("http://" + e.dataAddr + "/a/x"); err == nil {
		resp.Body.Close()
		t.Fatalf("an HTTP/2 request was answered: %s %d", resp.Proto, resp.StatusCode)
	}
	// TLS is not served.
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", e.dataAddr, &tls.Config{InsecureSkipVerify: true})
	if err == nil {
		conn.Close()
		t.Fatal("a TLS handshake succeeded")
	}

	// Request forms enforcement refuses at INGRESS, each with its event.
	for _, request := range []string{
		"GET /a/x HTTP/1.1\r\nHost: gw\r\nConnection: Upgrade, HTTP2-Settings\r\nUpgrade: h2c\r\nHTTP2-Settings: AAMAAABkAAQAAP__\r\n\r\n",
		"OPTIONS * HTTP/1.1\r\nHost: gw\r\nConnection: close\r\n\r\n",
		"GET http://" + e.a.addr + "/a/x HTTP/1.1\r\nHost: gw\r\nConnection: close\r\n\r\n",
		"CONNECT " + e.a.addr + " HTTP/1.1\r\nHost: " + e.a.addr + "\r\nConnection: close\r\n\r\n",
		"GET /a/x HTTP/1.1\r\nHost: gw\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n",
	} {
		resp, body := e.rawResponse(request)
		_, id := requestID(resp, body)
		if events := e.requestEvents(id, 1); resp.StatusCode != http.StatusBadRequest || len(events) != 1 || events[0]["stage"] != "INGRESS" {
			t.Errorf("%q: %d %s, events %v", strings.SplitN(request, "\r\n", 2)[0], resp.StatusCode, body, events)
		}
	}
	// Requests net/http itself refuses before any handler runs.
	for request, want := range map[string]int{
		"GET /a/x HTTP/1.1\r\nConnection: close\r\n\r\n":                                      http.StatusBadRequest,
		"GET /a/x HTTP/1.1\r\nHost: a\r\nHost: b\r\nConnection: close\r\n\r\n":                http.StatusBadRequest,
		"GET /a/x HTTP/1.1\r\nHost: gw\r\nX-Big: " + strings.Repeat("x", 16<<10) + "\r\n\r\n": http.StatusRequestHeaderFieldsTooLarge,
		"GET /a/x HTTP/2.1\r\nHost: gw\r\n\r\n":                                               http.StatusHTTPVersionNotSupported,
	} {
		if code, _ := e.raw(request); code != want {
			t.Errorf("%q: %d, want %d", strings.SplitN(request, "\r\n", 2)[0], code, want)
		}
	}
	e.assertReceipts(2, 0, 0)

	// The global body ceiling of 512 bytes, below the operation's 1024,
	// bounds the actual bytes of declared and chunked bodies alike.
	object := func(n int) string { return `{"p":"` + strings.Repeat("x", n-8) + `"}` }
	for _, tc := range []struct {
		size int
		want int
	}{{512, http.StatusOK}, {513, http.StatusRequestEntityTooLarge}} {
		document := object(tc.size)
		if code, _ := e.request(http.MethodPost, "/a/item", "application/json", document); code != tc.want {
			t.Errorf("declared %d-byte body: %d, want %d", tc.size, code, tc.want)
		}
		chunked := fmt.Sprintf("POST /a/item HTTP/1.1\r\nHost: gw\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n%x\r\n%s\r\n0\r\n\r\n", len(document), document)
		if code, _ := e.raw(chunked); code != tc.want {
			t.Errorf("chunked %d-byte body: %d, want %d", tc.size, code, tc.want)
		}
	}
	if got := e.a.receipts(); len(got) != 4 || len(got[2].Body) != 512 || len(got[3].Body) != 512 {
		t.Fatalf("receipts after the body limits: %d", len(got))
	}

	// A declared body that stops arriving ends once read_timeout has passed
	// since the request began: refused at BODY with its event, not handed on.
	started := time.Now()
	resp, body = e.rawResponse("POST /a/item HTTP/1.1\r\nHost: gw\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{\"p\":\"")
	elapsed := time.Since(started)
	_, id := requestID(resp, body)
	if events := e.requestEvents(id, 1); resp.StatusCode != http.StatusBadRequest || elapsed < 1500*time.Millisecond || elapsed > 5*time.Second ||
		len(events) != 1 || events[0]["stage"] != "BODY" || events[0]["reason"] != "read_failed" {
		t.Errorf("stalled body: %d %s after %s, events %v", resp.StatusCode, body, elapsed, events)
	}

	// A request header that does not arrive within read_header_timeout ends
	// the connection, and an idle kept connection ends after idle_timeout.
	for name, start := range map[string]string{
		"incomplete header": "GET /a/x HTTP/1.1\r\nHost: gw\r\n",
		"idle connection":   "GET /a/x HTTP/1.1\r\nHost: gw\r\n\r\n",
	} {
		conn, err := net.DialTimeout("tcp", e.dataAddr, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(conn, start)
		begun := time.Now()
		conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		reader := bufio.NewReader(conn)
		if name == "idle connection" {
			resp, err := http.ReadResponse(reader, nil)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			begun = time.Now()
		}
		_, err = reader.ReadByte()
		elapsed := time.Since(begun)
		conn.Close()
		if !errors.Is(err, io.EOF) || elapsed < 800*time.Millisecond || elapsed > 4*time.Second {
			t.Errorf("%s: closed after %s (%v), want about 1s", name, elapsed, err)
		}
	}
	e.assertReceipts(5, 0, 0)
}

// newPriorKnowledgeClient returns a client that speaks HTTP/2 over plain
// connections without negotiating it first.
func newPriorKnowledgeClient(t *testing.T) *http.Client {
	transport := &http.Transport{Protocols: new(http.Protocols)}
	transport.Protocols.SetUnencryptedHTTP2(true)
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport, Timeout: 30 * time.Second}
}

// A response that stalls after its header is cut off after the upstream
// read-idle bound, and a client that leaves cancels its upstream request:
// both through the native process, each recorded as what happened.
func TestNativeResponseStallAndCancellation(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.policy(e.a, true), seedOptions{})
	settings := e.nativeSettings()
	section(settings, "upstream")["read_idle_timeout"] = "1s"
	e.writeNative(settings)
	cancelled := make(chan string, 2)
	// A streamed response, which the proxy passes on as it arrives.
	e.a.handle = func(w http.ResponseWriter, r *http.Request) bool {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "0123456789")
		http.NewResponseController(w).Flush()
		<-r.Context().Done()
		cancelled <- "A"
		return true
	}
	e.b.handle = func(w http.ResponseWriter, r *http.Request) bool {
		<-r.Context().Done()
		cancelled <- "B"
		return true
	}
	e.start(binaries.plain)

	started := time.Now()
	resp, conn := e.openResponse("GET /a/stalls HTTP/1.1\r\nHost: gw\r\n\r\n")
	data, err := io.ReadAll(resp.Body)
	conn.Close()
	elapsed := time.Since(started)
	if resp.StatusCode != http.StatusOK || string(data) != "0123456789" || !errors.Is(err, io.ErrUnexpectedEOF) || elapsed > 4*time.Second {
		t.Fatalf("stalled response: %d %q %v after %s", resp.StatusCode, data, err, elapsed)
	}
	if who := <-cancelled; who != "A" {
		t.Fatalf("cancelled %s", who)
	}
	id := e.a.receipts()[0].RequestID
	if outcome := findEvent(e.eventsWhen(func(events []map[string]any) bool { return findEvent(events, "request.forwarded", id) != nil }), "request.forwarded", id); outcome == nil || outcome["error"] != "upstream_incomplete" {
		t.Fatalf("stall outcome %v", outcome)
	}

	conn, err = net.DialTimeout("tcp", e.dataAddr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(conn, "GET /b/waits HTTP/1.1\r\nHost: gw\r\n\r\n")
	waitFor(t, time.Now().Add(10*time.Second), "the upstream receipt", func() bool { return e.b.count() == 1 })
	conn.Close()
	select {
	case who := <-cancelled:
		if who != "B" {
			t.Fatalf("cancelled %s", who)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the upstream request was not cancelled after the client left")
	}
	id = e.b.receipts()[0].RequestID
	outcome := findEvent(e.eventsWhen(func(events []map[string]any) bool { return findEvent(events, "request.forwarded", id) != nil }), "request.forwarded", id)
	if outcome == nil || outcome["reason"] != "client_cancelled" || outcome["decision"] != "upstream_failure" {
		t.Fatalf("cancellation outcome %v", outcome)
	}
}

// The five controls run in the native process as in the shared enforcement
// pipeline: authentication against the one trusted key, roles held by the
// policy, the rate limit, body and header schemas under the body ceiling, the
// validated-header agreement and the gateway's own identity headers. Every
// refusal is recorded at its stage and reaches no upstream.
func TestNativeGatewayEnforcesControls(t *testing.T) {
	e := newEnv(t)
	p := e.protectedPolicy()
	for i := range p.Operations {
		switch p.Operations[i].ID {
		case "write-a":
			p.Operations[i].Body = jsonBody(64, `{"type":"object","additionalProperties":false,"required":["n"],"properties":{"n":{"type":"integer"}}}`)
			p.Operations[i].Headers = json.RawMessage(`{"type":"object","additionalProperties":true,"required":["X-Mode"],"properties":{"X-Mode":{"type":"string","enum":["fast","safe"]}}}`)
		case "read-b":
			p.Operations[i].Rate = &RateRule{Algorithm: "sliding-window", Count: 2, WindowSeconds: 60, Key: "subject"}
		}
	}
	seed(t, e.database, p, seedOptions{})
	e.start(binaries.plain)
	generation := e.status().Generation

	token := func(claims map[string]any) []string {
		return []string{"Authorization", "Bearer " + issuer.token(t, claims)}
	}
	expired, foreign, claimed := validClaims("alice"), validClaims("alice"), validClaims("mallory")
	expired["exp"] = time.Now().Add(-time.Minute).Unix()
	foreign["iss"] = "another-issuer"
	claimed["roles"] = []string{"UNKNOWN"}
	altered := issuer.tokenFor(t, "alice")
	altered = altered[:len(altered)-6] + "AAAAAA"
	asAlice := append(bearer(t, "alice"), "X-Mode", "fast")

	refusals := []struct {
		name, method, path, contentType, body string
		header                                []string
		status                                int
		stage                                 string
	}{
		{"no token", http.MethodGet, "/a/x", "", "", nil, http.StatusUnauthorized, "JWT"},
		{"expired token", http.MethodGet, "/a/x", "", "", token(expired), http.StatusUnauthorized, "JWT"},
		{"token of another issuer", http.MethodGet, "/a/x", "", "", token(foreign), http.StatusUnauthorized, "JWT"},
		{"altered signature", http.MethodGet, "/a/x", "", "", []string{"Authorization", "Bearer " + altered}, http.StatusUnauthorized, "JWT"},
		{"unknown role claimed in the verified token", http.MethodGet, "/a/x", "", "", token(claimed), http.StatusForbidden, "RBAC"},
		{"role without the permit", http.MethodPost, "/a/item", "application/json", `{"n":1}`, append(bearer(t, "bob"), "X-Mode", "fast"), http.StatusForbidden, "RBAC"},
		{"body outside its schema", http.MethodPost, "/a/item", "application/json", `{"n":"one"}`, asAlice, http.StatusBadRequest, "VALIDATION"},
		{"missing validated header", http.MethodPost, "/a/item", "application/json", `{"n":1}`, bearer(t, "alice"), http.StatusBadRequest, "VALIDATION"},
		{"validated header named in Connection", http.MethodPost, "/a/item", "application/json", `{"n":1}`, append(asAlice, "Connection", "X-Mode"), http.StatusBadRequest, "VALIDATION"},
		{"body above the operation ceiling", http.MethodPost, "/a/item", "application/json", `{"n":` + strings.Repeat("1", 64) + `}`, asAlice, http.StatusRequestEntityTooLarge, "BODY"},
		{"another media type", http.MethodPost, "/a/item", "text/plain", `{"n":1}`, asAlice, http.StatusUnsupportedMediaType, "BODY"},
		{"operation outside the policy", http.MethodGet, "/c/x", "", "", bearer(t, "alice"), http.StatusNotFound, "ROUTE"},
		{"denied operation", http.MethodGet, "/d", "", "", nil, http.StatusForbidden, "ACCESS"},
	}
	for _, r := range refusals {
		resp, body := e.exchange(r.method, r.path, r.contentType, r.body, r.header...)
		_, id := requestID(resp, body)
		events := e.requestEvents(id, 1)
		if resp.StatusCode != r.status || len(events) != 1 || events[0]["stage"] != r.stage || events[0]["kind"] != "request.rejected" {
			t.Errorf("%s: %d %s, events %v; want %d at %s", r.name, resp.StatusCode, body, events, r.status, r.stage)
		}
	}
	e.assertReceipts(0, 0, 0)

	// Admitted: the upstream receives the gateway's identity headers, not the
	// client's, with the validated header and body unchanged and no
	// credential.
	forged := append(asAlice, "X-Gateway-Subject", "bob", "X-Gateway-Roles", "ADMIN")
	if code, body := e.request(http.MethodPost, "/a/item", "application/json", `{"n":1}`, forged...); code != http.StatusOK || body != "A ok" {
		t.Fatalf("admitted write: %d %q", code, body)
	}
	r := e.a.receipts()[0]
	if r.Headers.Get("X-Gateway-Subject") != "alice" || r.Headers.Get("X-Gateway-Roles") != "WRITER" || r.Headers.Get("X-Mode") != "fast" ||
		r.Authorization || r.Body != `{"n":1}` || r.Generation != generation {
		t.Fatalf("upstream received %+v", r)
	}
	statuses := []int{}
	for range 3 {
		code, _ := e.get("/b/x", bearer(t, "bob")...)
		statuses = append(statuses, code)
	}
	if fmt.Sprint(statuses) != "[200 200 429]" {
		t.Fatalf("rate limit 2 per minute: %v", statuses)
	}
	e.assertReceipts(1, 2, 0)
	raw, _ := os.ReadFile(e.auditLog)
	if strings.Contains(string(raw), "Bearer") || strings.Contains(string(raw), altered[:40]) {
		t.Fatal("the audit log holds a credential")
	}
}

// A resource the gateway cannot acquire stops its startup with status 1 and a
// message naming it, and whatever it acquired is released again: no listener
// stays open and the database is free for its next owner. A database that
// another gateway holds is never shared.
func TestNativeStartupResourceFailures(t *testing.T) {
	occupy := func(t *testing.T, addr string) {
		l, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { l.Close() })
	}
	cases := []struct {
		name    string
		prepare func(t *testing.T, e *env) []string // returns the gateway's addresses that must stay closed
		cause   string
	}{
		{"data address in use", func(t *testing.T, e *env) []string { occupy(t, e.dataAddr); return []string{e.admin} }, "data listener"},
		{"admin address in use", func(t *testing.T, e *env) []string { occupy(t, e.admin); return []string{e.dataAddr} }, "admin listener"},
		{"audit log cannot be opened", func(t *testing.T, e *env) []string {
			if err := os.Mkdir(e.auditLog, 0o700); err != nil {
				t.Fatal(err)
			}
			return []string{e.dataAddr, e.admin}
		}, "audit log"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			seed(t, e.database, e.policy(e.a, false), seedOptions{})
			closed := tc.prepare(t, e)
			e.run(binaries.plain)
			if code := e.waitExit(); code != exitFailed || !strings.Contains(e.output.String(), tc.cause) {
				t.Fatalf("exit %d, want %d with %q\n%s", code, exitFailed, tc.cause, e.output)
			}
			for _, addr := range closed {
				if dialable(addr) {
					t.Fatalf("%s is open after the failed start", addr)
				}
			}
			st, err := openStore(e.database)
			if err != nil {
				t.Fatalf("the database is held: %v", err)
			}
			st.close()
		})
	}

	t.Run("database held by another gateway", func(t *testing.T) {
		e := newEnv(t)
		seed(t, e.database, e.policy(e.a, false), seedOptions{})
		e.start(binaries.plain)
		generation := e.status().Generation

		second := newEnv(t)
		settings := second.nativeSettings()
		section(settings, "gateway")["database"] = e.database
		second.writeNative(settings)
		second.run(binaries.plain)
		if code := second.waitExit(); code != exitFailed || !strings.Contains(second.output.String(), "held by another gateway process") {
			t.Fatalf("second gateway beside the first: exit %d\n%s", code, second.output)
		}
		if dialable(second.dataAddr) || dialable(second.admin) {
			t.Fatal("the refused gateway opened a listener")
		}
		if _, err := os.Stat(second.auditLog); !os.IsNotExist(err) {
			t.Fatalf("the refused gateway opened an audit log: %v", err)
		}
		if s := e.status(); s.Generation != generation {
			t.Fatalf("the first gateway changed: %+v", s)
		}

		// Once the first gateway has stopped, the second owns the deployment,
		// and the first is refused in its turn.
		e.stop()
		second.start(binaries.plain)
		if s := second.status(); s.Generation != generation || s.Admission != "ready" {
			t.Fatalf("second owner %+v, want generation %s", s, generation)
		}
		e.run(binaries.plain)
		if code := e.waitExit(); code != exitFailed || !strings.Contains(e.output.String(), "held by another gateway process") {
			t.Fatalf("first gateway beside the second: exit %d\n%s", code, e.output)
		}
		if code, body := second.get("/a/x"); code != http.StatusOK || body != "A ok" {
			t.Fatalf("second gateway after the refused start: %d %q", code, body)
		}
	})
}

// The native gateway owned in this process releases everything it acquired:
// after a stop, and after a start that fails part way. Concurrent stops share
// one result.
func TestNativeStartAndStopRelease(t *testing.T) {
	const dataServer = "gateway.startNative.func"
	e := newEnv(t)
	provisional := e.policy(e.a, true)
	seed(t, e.database, e.policy(e.a, false), seedOptions{provisional: &provisional, deadline: time.Now().Add(time.Hour)})
	e.writeNative(e.nativeSettings())
	cfg, err := loadNativeConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewJSONHandler(&logBuffer{}, nil))
	writers, admins, servers := goroutines(auditWriter), goroutines(adminServer), goroutines(dataServer)

	g, err := startNative(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	if code, body := e.get("/b/x"); code != http.StatusOK || body != "B ok" {
		t.Fatalf("provisional generation: %d %q", code, body)
	}
	errs := make([]error, 3)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Go(func() { errs[i] = g.stop() })
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("stop: %v", err)
		}
	}
	if g.controller.timer.Stop() {
		t.Fatal("the deadline timer was still armed after the stop")
	}
	if err := <-g.served; !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("data-plane Serve returned %v", err)
	}
	assertNativeReleased(t, e, writers, admins, servers)

	for name, addr := range map[string]string{"data": e.dataAddr, "admin": e.admin} {
		l, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		if g, err := startNative(cfg, logger); err == nil || !strings.Contains(err.Error(), name+" listener") {
			if g != nil {
				g.stop()
			}
			t.Fatalf("start with the %s address in use: %v", name, err)
		}
		l.Close()
		assertNativeReleased(t, e, writers, admins, servers)
	}
}

// assertNativeReleased fails unless no native gateway of e's deployment holds
// anything: its goroutines ended, its listeners closed and its database lock
// free.
func assertNativeReleased(t *testing.T, e *env, writers, admins, servers int) {
	t.Helper()
	waitGoroutines(t, auditWriter, writers)
	waitGoroutines(t, adminServer, admins)
	waitGoroutines(t, "gateway.startNative.func", servers)
	if dialable(e.dataAddr) || dialable(e.admin) || dialable(e.metrics) {
		t.Fatal("a listener is still open")
	}
	st, err := openStore(e.database)
	if err != nil {
		t.Fatalf("the database is still held: %v", err)
	}
	st.close()
}

// Listening is not admission. The startup output and the protected
// status say whether protected admission can run: not in bootstrap or
// recovery, without usable authority, with a failed audit log or a
// generation past its deadline, and not once the stop has begun. A confirmed
// deny-all base is ready, though it admits nothing. The unauthenticated
// health check stays a liveness and mode report.
func TestNativeAdmissionStatus(t *testing.T) {
	// unavailableAtCapture checks that a protected request is refused before
	// any policy decision and reaches no upstream.
	unavailableAtCapture := func(e *env) {
		e.t.Helper()
		resp, body := e.exchange(http.MethodGet, "/a/x", "", "")
		_, id := requestID(resp, body)
		if events := e.requestEvents(id, 1); resp.StatusCode != http.StatusServiceUnavailable || len(events) != 1 || events[0]["stage"] != "CAPTURE" {
			e.t.Fatalf("request: %d %s, events %v", resp.StatusCode, body, events)
		}
		e.assertReceipts(0, 0, 0)
	}

	t.Run("bootstrap", func(t *testing.T) {
		e := newEnv(t)
		if out, code := e.runOperator(binaries.plain, nil, "install", "--admin", "root"); code != 0 {
			t.Fatalf("install: %d %s", code, out)
		}
		e.start(binaries.plain)
		e.waitOutput(`"msg":"protected admission","admission":"bootstrap_locked"`)
		if health := e.health(); health != modeBootstrap {
			t.Fatalf("health %q", health)
		}
		unavailableAtCapture(e)
	})

	t.Run("recovery required", func(t *testing.T) {
		e := newEnv(t)
		seed(t, e.database, e.policy(e.a, false), seedOptions{mode: modeRecovery})
		e.start(binaries.plain)
		e.waitOutput(`"admission":"recovery_required"`)
		if s := e.status(); s.Admission != modeRecovery || s.State != modeRecovery || e.health() != modeRecovery {
			t.Fatalf("status %+v", s)
		}
		unavailableAtCapture(e)
	})

	for _, tc := range []struct {
		name    string
		prepare func(e *env)
		reason  string
	}{
		{"missing database", func(e *env) {}, "missing or empty"},
		{"earlier schema version", func(e *env) {
			seed(e.t, e.database, e.policy(e.a, false), seedOptions{version: databaseVersion - 1})
		}, "run its migrate command"},
		{"stored revision altered", func(e *env) {
			seed(e.t, e.database, e.policy(e.a, false), seedOptions{})
			db := inspect(e.t, e.database)
			if _, err := db.Exec(`UPDATE revision SET document = ? WHERE id = 1`, []byte(`{}`)); err != nil {
				e.t.Fatal(err)
			}
			db.Close()
		}, "checksum mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			tc.prepare(e)
			e.start(binaries.plain)
			e.waitOutput(`"msg":"protected admission","admission":"unavailable"`)
			if health := e.health(); health != "unavailable" {
				t.Fatalf("health %q", health)
			}
			if reason := e.unavailableReason(); !strings.Contains(reason, tc.reason) {
				t.Fatalf("recorded reason %q, want %q", reason, tc.reason)
			}
			unavailableAtCapture(e)
		})
	}

	t.Run("confirmed deny-all base", func(t *testing.T) {
		e := newEnv(t)
		seed(t, e.database, e.denyAllPolicy(), seedOptions{})
		e.start(binaries.plain)
		if s := e.status(); s.Admission != "ready" {
			t.Fatalf("status %+v", s)
		}
		for path, want := range map[string]int{"/a/x": http.StatusForbidden, "/b/x": http.StatusForbidden, "/zzz": http.StatusNotFound} {
			if code, _ := e.get(path); code != want {
				t.Errorf("%s: %d, want %d", path, code, want)
			}
		}
		e.assertReceipts(0, 0, 0)
	})

	t.Run("audit log failed while serving", func(t *testing.T) {
		e := newEnv(t)
		seed(t, e.database, e.policy(e.a, false), seedOptions{})
		e.start(binaries.fault, "GATEWAY_FAULT=audit-fail-outcome")
		if s := e.status(); s.Admission != "ready" {
			t.Fatalf("before the failure %+v", s)
		}
		if code, _ := e.get("/a/x"); code != http.StatusOK {
			t.Fatalf("first request: %d", code)
		}
		waitFor(t, time.Now().Add(10*time.Second), "the audit failure", func() bool { return e.status().AuditFailed })
		if s := e.status(); s.Admission != "audit_failed" || s.State != modeNormal || e.health() != modeNormal {
			t.Fatalf("after the failure %+v, health %q", s, e.health())
		}
		if code, _ := e.get("/a/x"); code != http.StatusServiceUnavailable {
			t.Fatalf("request with the failed audit log: %d", code)
		}
		e.assertReceipts(1, 0, 0)
	})

	t.Run("generation past its deadline", func(t *testing.T) {
		e := newEnv(t)
		provisional := e.policy(e.a, true)
		deadline := time.UnixMilli(time.Now().Add(1500 * time.Millisecond).UnixMilli())
		seed(t, e.database, e.policy(e.a, false), seedOptions{provisional: &provisional, deadline: deadline})
		e.start(binaries.fault, "GATEWAY_FAULT=no-deadline-timer")
		if s := e.status(); s.Admission != "ready" || !s.NotAfter.Equal(deadline) {
			t.Fatalf("provisional %+v", s)
		}
		time.Sleep(time.Until(deadline.Add(200 * time.Millisecond)))
		if s := e.status(); s.Admission != "generation_expired" || s.Provisional != 2 {
			t.Fatalf("past the deadline %+v", s)
		}
		unavailableAtCapture(e)
		// The refused request ends the provisional base; the restored one is
		// ready.
		waitFor(t, time.Now().Add(10*time.Second), "the rollback", func() bool { s := e.status(); return s.Admission == "ready" && s.Revision == 1 })
		if code, _ := e.get("/b/x"); code != http.StatusNotFound {
			t.Fatalf("operation of the rolled-back revision: %d", code)
		}
	})
}

// The native process stops gracefully on Ctrl+Break (os.Interrupt): from the
// signal on no new request is admitted and the protected status says it is
// stopping, while a request admitted before finishes; the listeners close,
// the process exits with status 0 and the next start serves the same
// generation. A second signal forces the exit with status 2.
func TestNativeGracefulStop(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.policy(e.a, false), seedOptions{})
	e.interruptible = true
	holdFile, release := filepath.Join(e.dir, "hold"), filepath.Join(e.dir, "release")
	e.start(binaries.fault, "GATEWAY_HOLD_PATH=/a/held", "GATEWAY_HOLD_FILE="+holdFile, "GATEWAY_HOLD_RELEASE="+release)
	generation := e.status().Generation

	idle, err := net.DialTimeout("tcp", e.dataAddr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	io.WriteString(idle, "GET /a/x HTTP/1.1\r\nHost: gw\r\n\r\n")
	idleReader := bufio.NewReader(idle)
	if resp, err := http.ReadResponse(idleReader, nil); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("request on the kept connection: %v", err)
	} else {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	held := make(chan string, 1)
	go func() {
		resp, err := (&http.Client{Timeout: 30 * time.Second}).Get("http://" + e.dataAddr + "/a/held")
		if err != nil {
			held <- err.Error()
			return
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		held <- fmt.Sprintf("%d %s", resp.StatusCode, body)
	}()
	waitForFile(t, holdFile)

	interrupt(t, e.process)
	e.waitOutput(`"msg":"stopping","signal":"interrupt"`)
	waitFor(t, time.Now().Add(5*time.Second), "the data listener to close", func() bool { return !dialable(e.dataAddr) })
	if s := e.status(); s.Admission != stateStopping || s.State != stateStopping {
		t.Fatalf("status while stopping %+v", s)
	}
	if health := e.health(); health != "unavailable" {
		t.Fatalf("health while stopping %q", health)
	}
	idle.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := idleReader.ReadByte(); !errors.Is(err, io.EOF) {
		t.Fatalf("the idle connection was not closed: %v", err)
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := <-held; got != "200 A ok" {
		t.Fatalf("request admitted before the stop: %s", got)
	}
	if code := e.waitExit(); code != exitStopped {
		t.Fatalf("exit status %d\n%s", code, e.output)
	}
	if !strings.Contains(e.output.String(), `"msg":"stopped"`) || dialable(e.admin) {
		t.Fatalf("stop not completed\n%s", e.output)
	}
	e.assertReceipts(2, 0, 0)

	e.interruptible = false
	e.start(binaries.plain)
	if s := e.status(); s.Generation != generation {
		t.Fatalf("generation %s after the restart, want %s", s.Generation, generation)
	}
	if countKind(e.auditEvents(), "audit.recovered") != 0 {
		t.Fatal("the restart found an incomplete audit record")
	}

	t.Run("second signal", func(t *testing.T) {
		e := newEnv(t)
		seed(t, e.database, e.policy(e.a, false), seedOptions{})
		e.interruptible = true
		holdFile, release := filepath.Join(e.dir, "hold"), filepath.Join(e.dir, "release")
		e.start(binaries.fault, "GATEWAY_HOLD_PATH=/a/held", "GATEWAY_HOLD_FILE="+holdFile, "GATEWAY_HOLD_RELEASE="+release)
		go (&http.Client{Timeout: 30 * time.Second}).Get("http://" + e.dataAddr + "/a/held")
		waitForFile(t, holdFile)
		interrupt(t, e.process)
		e.waitOutput(`"msg":"stopping"`)
		interrupt(t, e.process)
		if code := e.waitExit(); code != exitForced || !strings.Contains(e.output.String(), "forced exit") {
			t.Fatalf("exit status %d after two signals\n%s", code, e.output)
		}
		if strings.Contains(e.output.String(), `"msg":"stopped"`) {
			t.Fatal("a forced exit was reported as a completed stop")
		}
	})
}

// The native data plane serves the Client website itself: the page
// at the approved Fetch routes and flat assets directory, with its security headers,
// matched on the exact target. Every other request, including every other
// spelling of a website path, goes to enforcement, and no request reaches a
// file outside the website or the admin dashboard. Without the setting there
// is no website.
func TestNativeWebsite(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.policy(e.a, false), seedOptions{})
	site := writeDashboard(t)
	settings := e.nativeSettings()
	section(settings, "data")["website"] = site
	e.writeNative(settings)
	e.start(binaries.plain)

	headers := map[string]string{
		"Content-Security-Policy": uiPolicy,
		"X-Frame-Options":         "DENY",
		"Referrer-Policy":         "no-referrer",
		"Cache-Control":           "no-store",
		"X-Content-Type-Options":  "nosniff",
	}
	for _, f := range []struct{ path, contentType, body string }{
		{"/", "text/html; charset=utf-8", "<!doctype html><title>dashboard</title>"},
		{"/my-orders/ORD-1001?from=link", "text/html; charset=utf-8", "<!doctype html><title>dashboard</title>"},
		{"/assets/app.js", "text/javascript", "console.log('app')"},
		{"/assets/app.css", "text/css", "body{}"},
		{"/assets/logo.svg", "image/svg+xml", "<svg xmlns=\"http://www.w3.org/2000/svg\"/>"},
	} {
		resp, body := e.exchange(http.MethodGet, f.path, "", "")
		if resp.StatusCode != http.StatusOK || body != f.body || resp.Header.Get("Content-Type") != f.contentType {
			t.Errorf("GET %s: %d %q %q", f.path, resp.StatusCode, resp.Header.Get("Content-Type"), body)
			continue
		}
		for name, want := range headers {
			if got := resp.Header.Get(name); got != want {
				t.Errorf("GET %s: %s = %q, want %q", f.path, name, got, want)
			}
		}
	}
	if resp, body := e.exchange(http.MethodHead, "/", "", ""); resp.StatusCode != http.StatusOK || body != "" {
		t.Errorf("HEAD /: %d %q", resp.StatusCode, body)
	}
	if code, body := e.get("/assets/missing.js"); code != http.StatusNotFound || strings.Contains(body, "request_id") {
		t.Errorf("missing asset: %d %q", code, body)
	}
	// A Windows device name is no file of the website: os.DirFS refuses it,
	// so the request is answered 404 at once instead of opening the device.
	if code, body := e.get("/assets/CON.js"); code != http.StatusNotFound || strings.Contains(body, "request_id") {
		t.Errorf("device name as an asset: %d %q", code, body)
	}

	// Everything else is the policy's: refused here by enforcement, with an
	// event, and never answered with a file.
	files := []string{"dashboard", "console.log", "body{}", "<svg", "not for the browser", "nested", "hidden", "other", "SQLite"}
	for _, probe := range []struct{ method, target string }{
		{http.MethodGet, "/demo"}, {http.MethodGet, "/demo/"}, {http.MethodGet, "/index.html"}, {http.MethodGet, "/assets/"},
		{http.MethodGet, "/assets/app.js.map"}, {http.MethodGet, "/assets/nested/x.js"}, {http.MethodGet, "/assets/readme.txt"},
		{http.MethodGet, "/assets/upper.JS.html"}, {http.MethodGet, "/secret.txt"}, {http.MethodGet, "/%61ssets/app.js"},
		{http.MethodGet, "/assets/..%2fsecret.txt"}, {http.MethodGet, "/assets/%2e%2e%2fsecret.txt"},
		{http.MethodGet, "/assets/../../secret.txt"}, {http.MethodGet, "/assets/..\\secret.txt"},
		{http.MethodGet, "/food//"}, {http.MethodGet, "//food"}, {http.MethodGet, "/FOOD"}, {http.MethodGet, "/food/%2e"},
		{http.MethodGet, "/assets/" + filepath.Base(e.database)}, {http.MethodGet, "/demo/assets/app.js"},
		{http.MethodGet, "/orders"}, {http.MethodGet, "/orders/ORD-1001"}, {http.MethodGet, "/support/tickets"},
		{http.MethodPost, "/"}, {http.MethodPut, "/assets/app.js"}, {http.MethodOptions, "/"},
	} {
		resp, body := e.rawResponse(probe.method + " " + probe.target + " HTTP/1.1\r\nHost: gw\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		_, id := requestID(resp, body)
		events := e.requestEvents(id, 1)
		if resp.StatusCode == http.StatusOK || id == "" || len(events) != 1 || resp.Header.Get("Content-Security-Policy") != "" {
			t.Errorf("%s %s: %d %q, events %v", probe.method, probe.target, resp.StatusCode, body, events)
		}
		for _, content := range files {
			if strings.Contains(body, content) {
				t.Errorf("%s %s answered with file content %q", probe.method, probe.target, body)
			}
		}
	}
	if code, body, _ := e.adminSend(http.MethodGet, "/demo/", ""); code != http.StatusNotFound || strings.Contains(body, "dashboard") {
		t.Errorf("the admin listener serves the website: %d %q", code, body)
	}
	e.assertReceipts(0, 0, 0)

	e.stop()
	e.writeNative(e.nativeSettings())
	e.start(binaries.plain)
	resp, body := e.exchange(http.MethodGet, "/", "", "")
	if _, id := requestID(resp, body); resp.StatusCode != http.StatusNotFound || id == "" {
		t.Errorf("/ without a website: %d %q", resp.StatusCode, body)
	}
}
