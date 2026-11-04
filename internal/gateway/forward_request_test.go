// Forwarded request integrity, metadata and bodies.

package gateway

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

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
