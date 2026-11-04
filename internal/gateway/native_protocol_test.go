// Native data-plane protocol, stalls and cancellation.

package gateway

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

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
