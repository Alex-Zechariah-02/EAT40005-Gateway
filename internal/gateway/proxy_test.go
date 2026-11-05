package gateway

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

// An admitted request reaches the upstream with its method, target,
// query, body and ordinary headers intact, without the client credential and
// with the gateway's own correlation headers; the upstream response returns
// to the client unchanged.
func TestForwardedRequestAndResponseRoundTrip(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.policy(e.a, false), seedOptions{})
	e.a.handle = func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("X-Upstream", "kept")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"ok":true}`)
		return true
	}
	e.start(binaries.plain)
	g := e.status().Generation

	resp, body := e.exchange(http.MethodPost, "/a/item?q=1&q=2&x=%20y", "application/json; charset=utf-8", `{"n":1}`,
		"X-Custom", "kept", "Accept", "application/json", "Authorization", "Bearer secret-token")
	if resp.StatusCode != http.StatusCreated || resp.Header.Get("X-Upstream") != "kept" || body != `{"ok":true}` {
		t.Fatalf("response %d %v %q", resp.StatusCode, resp.Header, body)
	}

	got := e.a.receipts()
	if len(got) != 1 {
		t.Fatalf("%d receipts, want 1", len(got))
	}
	r := got[0]
	h := r.Headers
	switch {
	case r.Method != http.MethodPost || r.Target != "/a/item?q=1&q=2&x=%20y" || r.Body != `{"n":1}`:
		t.Errorf("request line or body changed: %s %s %q", r.Method, r.Target, r.Body)
	case h.Get("Content-Type") != "application/json; charset=utf-8" || h.Get("Content-Length") != "7":
		t.Errorf("entity headers changed: %v", h)
	case h.Get("X-Custom") != "kept" || h.Get("Accept") != "application/json":
		t.Errorf("ordinary headers not preserved: %v", h)
	case r.Authorization:
		t.Error("client credential reached the upstream")
	case r.Generation != g || len(r.RequestID) != 32:
		t.Errorf("gateway headers: generation %q request id %q", r.Generation, r.RequestID)
	}
	e.assertReceipts(1, 0, 0)
}

// The forwarding metadata of an admitted request, as the upstream and the
// client receive it: the Host and the raw query are kept; hop-by-hop fields
// are removed, except a TE that asks for trailers; the X-Forwarded-* fields
// carry the proxy's own view, never the client's values; Via names the
// gateway after any earlier hop. A client's Forwarded field is removed, and
// the upstream is told to close its one-request connection. The response
// keeps the upstream's Server field, loses hop-by-hop fields and gains Via.
func TestForwardedRequestMetadata(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.policy(e.a, false), seedOptions{})
	e.a.handle = func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Server", "upstream-server")
		w.Header().Set("Keep-Alive", "timeout=99")
		io.WriteString(w, "A ok")
		return true
	}
	e.start(binaries.plain)

	resp, body := e.rawResponse("GET /a/x?b=1;c=2&d=%zz HTTP/1.1\r\nHost: gw.example:8443\r\n" +
		"X-Forwarded-For: 203.0.113.9\r\nX-Forwarded-Host: evil.example\r\nX-Forwarded-Proto: https\r\n" +
		"Forwarded: for=203.0.113.9;host=evil.example\r\nVia: 1.0 client-proxy\r\n" +
		"Keep-Alive: timeout=5\r\nProxy-Connection: keep-alive\r\nTE: trailers\r\nConnection: close\r\n\r\n")
	if resp.StatusCode != http.StatusOK || body != "A ok" {
		t.Fatalf("response %d %q", resp.StatusCode, body)
	}
	e.assertReceipts(1, 0, 0)
	got := e.a.receipts()[0]
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
		{"Via", strings.Join(h.Values("Via"), "|"), "1.0 client-proxy|1.1 gateway"},
		{"Te", h.Get("Te"), "trailers"},
		{"Keep-Alive", h.Get("Keep-Alive"), ""},
		{"Proxy-Connection", h.Get("Proxy-Connection"), ""},
		{"Connection", h.Get("Connection"), "close"},
		{"response Via", strings.Join(resp.Header.Values("Via"), "|"), "1.1 gateway"},
		{"response Server", resp.Header.Get("Server"), "upstream-server"},
		{"response Keep-Alive", resp.Header.Get("Keep-Alive"), ""},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s: %q, want %q", c.name, c.got, c.want)
		}
	}
}

// Nothing in the request can choose the destination.
func TestClientCannotSelectUpstream(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.policy(e.a, false), seedOptions{})
	e.start(binaries.plain)

	decoy := e.c.addr
	attempts := []string{
		"GET /a/x HTTP/1.1\r\nHost: " + decoy + "\r\n",
		"GET /a/x HTTP/1.1\r\nHost: gw\r\nX-Forwarded-Host: " + decoy + "\r\nX-Forwarded-Proto: https\r\nForwarded: host=" + decoy + "\r\n",
		"GET /a/x HTTP/1.1\r\nHost: gw\r\nX-Gateway-Upstream: C\r\nX-Gateway-Dial: " + decoy + "\r\nX-Gateway-Operation: denied\r\n",
		"GET /a/x?upstream=C&dial=" + decoy + "&operation=read-b HTTP/1.1\r\nHost: gw\r\n",
		"GET /a/" + decoy + " HTTP/1.1\r\nHost: gw\r\n",
	}
	for _, request := range attempts {
		code, _ := e.raw(request + "Connection: close\r\n\r\n")
		if code != http.StatusOK {
			t.Errorf("%q: status %d, want 200 from the bound upstream", request, code)
		}
	}
	e.assertReceipts(len(attempts), 0, 0)
	for _, r := range e.a.receipts() {
		for name := range r.Headers {
			if strings.HasPrefix(name, "X-Gateway-") && name != "X-Gateway-Generation" {
				t.Errorf("client header %s reached the upstream", name)
			}
		}
	}

	// An absolute-form target naming the decoy is refused before routing.
	if code, _ := e.raw("GET http://" + decoy + "/a/x HTTP/1.1\r\nHost: " + decoy + "\r\nConnection: close\r\n\r\n"); code != http.StatusBadRequest {
		t.Errorf("absolute-form target: %d", code)
	}
	e.assertReceipts(len(attempts), 0, 0)
}

// policyWithUpstream adds operation GET /z, served by its own upstream.
func (e *env) policyWithUpstream(dial string) policy.Policy {
	p := e.policy(e.a, false)
	p.Upstreams = append(p.Upstreams, policy.Upstream{ID: "Z", Dial: dial})
	p.APIs = append(p.APIs, policy.API{ID: "z", Upstream: "Z"})
	p.Operations = append(p.Operations, policy.Operation{ID: "read-z", API: "z", Method: "GET", Target: "/z", Access: "allow", Auth: "none"})
	return p
}

// Upstream failures produce bounded, safe errors and never another
// destination.
func TestUpstreamFailuresAreContained(t *testing.T) {
	t.Run("connection refused", func(t *testing.T) {
		e := newEnv(t)
		closed := freeAddr(t)
		seed(t, e.database, e.policyWithUpstream(closed), seedOptions{})
		e.start(binaries.plain)
		code, body := e.get("/z")
		if code != http.StatusBadGateway || !strings.Contains(body, `"upstream_unavailable"`) || strings.Contains(body, port(closed)) {
			t.Fatalf("refused upstream: %d %s", code, body)
		}
		e.assertReceipts(0, 0, 0)
	})

	t.Run("response timeout", func(t *testing.T) {
		e := newEnv(t)
		seed(t, e.database, e.policy(e.a, false), seedOptions{})
		e.writeConfig(configOptions{headerTimeout: "1s"})
		release := make(chan struct{})
		defer close(release)
		e.a.handle = func(w http.ResponseWriter, r *http.Request) bool {
			select {
			case <-release:
			case <-time.After(5 * time.Second):
			}
			return false
		}
		e.start(binaries.plain)
		started := time.Now()
		code, body := e.get("/a/x")
		elapsed := time.Since(started)
		if code != http.StatusGatewayTimeout || !strings.Contains(body, `"upstream_timeout"`) || elapsed > 3*time.Second {
			t.Fatalf("slow upstream: %d %s after %v", code, body, elapsed)
		}
		// The request did arrive; the gateway cannot undo what the upstream did.
		e.assertReceipts(1, 0, 0)
	})

	hijacked := func(reply string) func(w http.ResponseWriter, r *http.Request) bool {
		return func(w http.ResponseWriter, r *http.Request) bool {
			conn, buffered, _ := http.NewResponseController(w).Hijack()
			buffered.WriteString(reply)
			buffered.Flush()
			conn.Close()
			return true
		}
	}

	t.Run("malformed response", func(t *testing.T) {
		e := newEnv(t)
		seed(t, e.database, e.policy(e.a, false), seedOptions{})
		e.a.handle = hijacked("this is not HTTP\r\n\r\n")
		e.start(binaries.plain)
		if code, body := e.get("/a/x"); code != http.StatusBadGateway || !strings.Contains(body, `"upstream_unavailable"`) {
			t.Fatalf("malformed upstream response: %d %s", code, body)
		}
		e.assertReceipts(1, 0, 0)
	})

	t.Run("truncated response", func(t *testing.T) {
		e := newEnv(t)
		seed(t, e.database, e.policy(e.a, false), seedOptions{})
		e.a.handle = hijacked("HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\n0123456789")
		e.start(binaries.plain)

		conn, err := net.Dial("tcp", e.dataAddr)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(10 * time.Second))
		fmt.Fprint(conn, "GET /a/x HTTP/1.1\r\nHost: gw\r\n\r\n")
		// The response is aborted: a head the gateway had not yet sent never
		// arrives, and a started body ends early.
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err == nil {
			received, readErr := io.ReadAll(resp.Body)
			t.Logf("truncated upstream body: client got status %d, %d of %d bytes, read error %v", resp.StatusCode, len(received), resp.ContentLength, readErr)
			if readErr == nil || len(received) >= 100 {
				t.Fatal("the client was not shown that the response is incomplete")
			}
		} else if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("truncated upstream body: %v", err)
		} else {
			t.Logf("truncated upstream body: the connection closed before a response (%v)", err)
		}
		e.assertReceipts(1, 0, 0)

		// The proxy aborts the response, but the outcome is still recorded
		// as a failure rather than lost.
		events := e.eventsWhen(func(events []map[string]any) bool { return countKind(events, "request.forwarded") == 1 })
		var outcome map[string]any
		for _, ev := range events {
			if ev["kind"] == "request.forwarded" {
				outcome = ev
			}
		}
		if outcome == nil {
			t.Fatal("no outcome event for the aborted response")
		}
		stages, _ := outcome["stages"].(map[string]any)
		if outcome["decision"] != "upstream_failure" || outcome["error"] != "upstream_incomplete" || stages["FORWARD"] != "FAIL" {
			t.Fatalf("outcome event %v", outcome)
		}
	})
}

// A request that may have changed state upstream is not replayed: the
// gateway sends every request once, a read as well as a write.
func TestRetriesNeverReplayWrites(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.policy(e.a, false), seedOptions{})
	e.a.handle = func(w http.ResponseWriter, r *http.Request) bool {
		// Read the request, then drop the connection without answering.
		conn, _, _ := http.NewResponseController(w).Hijack()
		conn.Close()
		return true
	}
	e.start(binaries.plain)

	if code, _ := e.request(http.MethodPost, "/a/item", "application/json", `{"n":1}`); code != http.StatusBadGateway {
		t.Fatalf("interrupted write: %d", code)
	}
	e.assertReceipts(1, 0, 0)

	// A read with the same failure is not resent either.
	e.resetReceivers()
	if code, _ := e.get("/a/x"); code != http.StatusBadGateway {
		t.Fatalf("interrupted read: %d", code)
	}
	e.assertReceipts(1, 0, 0)
}
