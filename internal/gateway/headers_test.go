// Header schema refusals, authentication and kept connections.

package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// A header schema may not declare a field whose validated value the upstream
// would not receive: the credential, hop-by-hop fields, Host, the body length
// and the forwarding fields. A publication declaring one is refused and the
// current generation stays.
func TestHeaderSchemaRefusesUnforwardedFields(t *testing.T) {
	e := newEnv(t)
	c, _ := e.serveNative(e.keywordPolicy(), forwardTimeouts)
	current := c.Current().ID
	for i, name := range []string{"Via", "Connection", "Content-Length", "X-Forwarded-For"} {
		p := e.keywordPolicy()
		p.Operations[0].Headers = json.RawMessage(`{"type":"object","additionalProperties":true,"properties":{"` + name + `":{"type":"string"}}}`)
		outcome, status := c.Publish(context.Background(), "test", Change{ID: fmt.Sprintf("unforwarded-%d", i), Expected: 1, Policy: mustJSON(p)})
		if status == http.StatusOK || outcome.Outcome != "rejected" || outcome.Reason != "INVALID_VALUE" ||
			outcome.Field != "operations[0].headers.properties."+name || c.Current().ID != current {
			t.Errorf("%s: %d %+v, generation %s", name, status, outcome, c.Current().ID)
		}
		t.Logf("%s declared: %d %s %s at %s", name, status, outcome.Outcome, outcome.Reason, outcome.Field)
	}
	// The same operation with an end-to-end field is published.
	p := e.keywordPolicy()
	p.Operations[0].Headers = json.RawMessage(`{"type":"object","additionalProperties":true,"required":["Accept"],"properties":{"Accept":{"type":"string","enum":["application/json"]}}}`)
	if outcome, status := c.Publish(context.Background(), "test", Change{ID: "end-to-end", Expected: 1, Policy: mustJSON(p)}); status != http.StatusOK {
		t.Fatalf("end-to-end field: %d %+v", status, outcome)
	}
	if code, body := e.raw("GET /a/x HTTP/1.1\r\nHost: gw\r\nConnection: close\r\nAccept: application/json\r\n\r\n"); code != http.StatusOK || body != "A ok" ||
		e.a.receipts()[0].Headers.Get("Accept") != "application/json" {
		t.Fatalf("request under the published schema: %d %q", code, body)
	}
	if code, _ := e.raw("GET /a/x HTTP/1.1\r\nHost: gw\r\nConnection: close, Accept\r\nAccept: application/json\r\n\r\n"); code != http.StatusBadRequest {
		t.Fatalf("validated Accept named in Connection: %d", code)
	}
	e.assertReceipts(1, 0, 0)
}

// Authentication and role authorisation stay authoritative around header
// validation: a subject permitted the write reaches the upstream with the
// validated header, the media type and the gateway's identity fields in place
// of forged ones; a subject without the permit, and a permitted subject naming
// a validated field in Connection, reach nothing.
func TestValidatedHeadersWithAuthentication(t *testing.T) {
	e := newEnv(t)
	p := e.protectedPolicy()
	for i := range p.Operations {
		if p.Operations[i].ID == "write-a" {
			p.Operations[i].Headers = json.RawMessage(`{"type":"object","additionalProperties":true,"required":["Idempotency-Key"],"properties":{"Idempotency-Key":{"type":"string","minLength":8}}}`)
		}
	}
	e.serveNative(p, forwardTimeouts)
	write := func(subject, lines string) (int, string) {
		return e.rawExchange(fmt.Sprintf("POST /a/item HTTP/1.1\r\nHost: gw\r\nConnection: close\r\nAuthorization: Bearer %s\r\n"+
			"Content-Type: application/json\r\nIdempotency-Key: k-123456\r\n%sContent-Length: %d\r\n\r\n%s",
			issuer.tokenFor(t, subject), lines, len(keywordItem), keywordItem), false)
	}

	forged := "X-Gateway-Subject: bob\r\nX-Gateway-Roles: ADMIN\r\nX-Forwarded-For: 203.0.113.9\r\n"
	if status, body := write("alice", forged); status != http.StatusOK || e.a.count() != 1 {
		t.Fatalf("permitted write: %d %s with %d receipts", status, body, e.a.count())
	}
	r := e.a.receipts()[0]
	want := map[string]string{
		"Idempotency-Key": "k-123456", "Content-Type": "application/json", "X-Gateway-Subject": "alice",
		"X-Gateway-Roles": "WRITER", "X-Forwarded-For": "127.0.0.1", "Authorization": "",
	}
	for name, value := range want {
		if got := strings.Join(r.Headers.Values(name), "|"); got != value {
			t.Errorf("upstream received %s %q, want %q", name, got, value)
		}
	}
	if r.Body != keywordItem {
		t.Errorf("upstream received body %q", r.Body)
	}

	refused := []struct {
		name, subject, lines string
		status               int
		stage, reason        string
	}{
		{"subject without the permit", "bob", "", 403, "RBAC", "no_permit"},
		{"validated header named in Connection", "alice", "Connection: Idempotency-Key\r\n", 400, "VALIDATION", "connection_nominated"},
		{"media type named in Connection", "alice", "Connection: content-type\r\n", 400, "VALIDATION", "connection_nominated"},
	}
	for _, tc := range refused {
		status, body := write(tc.subject, tc.lines)
		_, id := requestID(&http.Response{StatusCode: status}, body)
		ev := e.eventOf(id, 1)
		reason, _ := ev["reason"].(string)
		if status != tc.status || ev["stage"] != tc.stage || reason != tc.reason {
			t.Errorf("%s: %d %s, event %v, want %d at %s (%s)", tc.name, status, body, ev, tc.status, tc.stage, tc.reason)
		}
		t.Logf("%s: %d, %s at %s (%s), receipts A %d", tc.name, status, ev["kind"], ev["stage"], reason, e.a.count())
	}
	e.assertReceipts(1, 0, 0)
}

// The correction leaves ordinary use unchanged: a client connection kept
// alive carries an admitted request, a refused nomination and an admitted
// write in turn, and the upstream receives the two admitted ones.
func TestValidatedHeadersOnKeptConnection(t *testing.T) {
	e := newEnv(t)
	e.serveNative(e.keywordPolicy(), forwardTimeouts)
	conn, err := net.DialTimeout("tcp", e.dataAddr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	reader := bufio.NewReader(conn)
	requests := []struct {
		request string
		status  int
	}{
		{"GET /a/probe HTTP/1.1\r\nHost: gw\r\nConnection: keep-alive\r\nX-Probe: on\r\n\r\n", http.StatusOK},
		{"GET /a/probe HTTP/1.1\r\nHost: gw\r\nConnection: keep-alive, X-Probe\r\nX-Probe: on\r\n\r\n", http.StatusBadRequest},
		{strings.Replace(itemRequest(""), "Connection: close", "Connection: keep-alive", 1), http.StatusOK},
	}
	for i, r := range requests {
		io.WriteString(conn, r.request)
		resp, err := http.ReadResponse(reader, nil)
		if err != nil {
			t.Fatalf("request %d on the kept connection: %v", i+1, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != r.status || resp.Close {
			t.Fatalf("request %d: %d (close %v), want %d on the same connection", i+1, resp.StatusCode, resp.Close, r.status)
		}
	}
	got := e.a.receipts()
	if len(got) != 2 || got[0].Headers.Get("X-Probe") != "on" || got[1].Body != keywordItem || got[1].Headers.Get("Idempotency-Key") != "k-123456" {
		t.Fatalf("receipts %+v", got)
	}
}
