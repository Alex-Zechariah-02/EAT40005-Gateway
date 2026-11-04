// Body ceiling on actual bytes.

package gateway

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// sizedBody returns a JSON object of exactly size bytes whose only member
// starts with tag, so that concurrent bodies can be told apart.
func sizedBody(tag string, size int) string {
	return `{"p":"` + tag + strings.Repeat("x", size-8-len(tag)) + `"}`
}

// chunkedBody encodes body in chunks of at most size bytes, followed by the
// last chunk.
func chunkedBody(body string, size int) string {
	var b strings.Builder
	for len(body) > 0 {
		n := min(size, len(body))
		fmt.Fprintf(&b, "%x\r\n%s\r\n", n, body[:n])
		body = body[n:]
	}
	b.WriteString("0\r\n\r\n")
	return b.String()
}

// sendBody posts a JSON body to path with the named framing and returns the
// status and reply. "declared" and "chunked" are HTTP/1.1 requests written
// byte for byte, the chunked body in chunks of at most 4096 bytes. header
// holds name/value pairs.
func (e *env) sendBody(framing, path, body string, header ...string) (int, string) {
	e.t.Helper()
	head := "POST " + path + " HTTP/1.1\r\nHost: gw\r\nConnection: close\r\nContent-Type: application/json\r\n"
	for i := 0; i+1 < len(header); i += 2 {
		head += header[i] + ": " + header[i+1] + "\r\n"
	}
	if framing == "declared" {
		return e.rawExchange(fmt.Sprintf("%sContent-Length: %d\r\n\r\n%s", head, len(body), body), false)
	}
	return e.rawExchange(head+"Transfer-Encoding: chunked\r\n\r\n"+chunkedBody(body, 4096), false)
}

// bodyFramings are the request framings the data plane serves for a body:
// HTTP/1.1 with a declared length or chunked.
var bodyFramings = []string{"declared", "chunked"}

// sizeRefusal returns the stage and reason expected when a body one byte over
// the effective ceiling is refused: a declared length above the global
// ceiling is refused at ingress, before any policy work; every other excess
// when the body is read.
func sizeRefusal(framing string, size, global int) (string, string) {
	if framing == "declared" && size > global {
		return "INGRESS", ""
	}
	return "BODY", "too_large"
}

// The effective body ceiling is the smaller of the global ceiling (16384
// bytes in the test configuration) and the operation's own, and it bounds the
// bytes actually read whatever the framing: a declared length or a chunked
// body. A body of exactly the ceiling reaches the upstream whole, with a
// declared length; one byte more is refused with 413 and reaches no upstream.
func TestBodyCeilingAppliesToActualBytes(t *testing.T) {
	e := newEnv(t)
	p := e.policy(e.a, false)
	p.Operations = append(p.Operations,
		Operation{ID: "large", API: "a", Method: "POST", Target: "/a/large", Access: "allow", Auth: "none", Body: jsonBody(32768, anyObject)},
		Operation{ID: "small", API: "a", Method: "POST", Target: "/a/small", Access: "allow", Auth: "none", Body: jsonBody(8192, anyObject)},
		Operation{ID: "equal", API: "a", Method: "POST", Target: "/a/equal", Access: "allow", Auth: "none", Body: jsonBody(16384, anyObject)},
	)
	seed(t, e.database, p, seedOptions{})
	e.start(binaries.plain)
	const global = 16384
	routed := []string{"INGRESS", "CAPTURE", "CANONICAL", "ROUTE", "ACCESS"}

	operations := []struct {
		name, path string
		ceiling    int
	}{
		{"global below the operation", "/a/large", global},
		{"operation below the global", "/a/small", 8192},
		{"equal ceilings", "/a/equal", global},
	}
	forwarded := 0
	for _, op := range operations {
		for _, framing := range bodyFramings {
			for _, size := range []int{op.ceiling, op.ceiling + 1} {
				name := fmt.Sprintf("%s, %s, %d bytes", op.name, framing, size)
				body := sizedBody("", size)
				status, reply := e.sendBody(framing, op.path, body)
				if size == op.ceiling {
					forwarded++
					receipts := e.a.receipts()
					if status != http.StatusOK || len(receipts) != forwarded {
						t.Fatalf("%s: status %d (%s), %d receipts, want 200 and %d", name, status, reply, len(receipts), forwarded)
					}
					last := receipts[forwarded-1]
					if last.Body != body || last.Headers.Get("Content-Length") != strconv.Itoa(size) || len(last.Trailer) != 0 {
						t.Errorf("%s: upstream received %d bytes (intact %v) with Content-Length %q",
							name, len(last.Body), last.Body == body, last.Headers.Get("Content-Length"))
					}
					continue
				}

				stage, reason := sizeRefusal(framing, size, global)
				want := expectedStages(nil, "INGRESS", stageReject)
				if stage == "BODY" {
					want = public(expectedStages(routed, "BODY", stageReject))
				}
				_, id := requestID(&http.Response{StatusCode: status}, reply)
				events := e.requestEvents(id, 1)
				if status != http.StatusRequestEntityTooLarge || len(events) != 1 || events[0]["kind"] != "request.rejected" ||
					events[0]["stage"] != stage || events[0]["status"] != float64(413) || events[0]["error"] != "payload_too_large" ||
					!sameStages(events[0]["stages"], want) {
					t.Errorf("%s: status %d (%s), events %v, want 413 at %s", name, status, reply, events, stage)
				} else if got, _ := events[0]["reason"].(string); got != reason {
					t.Errorf("%s: reason %q, want %q", name, got, reason)
				}
				if n := e.a.count(); n != forwarded {
					t.Fatalf("%s: %d receipts, want %d: a refused body was forwarded", name, n, forwarded)
				}
			}
		}
	}

	// A chunked body that announces more than the ceiling and stalls one byte
	// past it is refused at once: the read stops at the ceiling instead of
	// waiting for the rest, and the connection is closed.
	conn, err := net.DialTimeout("tcp", e.dataAddr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	started := time.Now()
	io.WriteString(conn, "POST /a/large HTTP/1.1\r\nHost: gw\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n"+
		fmt.Sprintf("%x\r\n", 20000)+sizedBody("", 20000)[:global+1])
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("stalled chunked body: %v", err)
	}
	resp.Body.Close()
	if elapsed := time.Since(started); resp.StatusCode != http.StatusRequestEntityTooLarge || !resp.Close || elapsed > 5*time.Second {
		t.Errorf("stalled chunked body: %d after %v, connection closed %v; want 413 at once with the connection closed", resp.StatusCode, elapsed, resp.Close)
	}

	// Framing as the HTTP server reads it: an invalid chunk fails the read; a
	// chunked body overrides a declared length (RFC 9112 section 6.3), so a
	// small declared length cannot hide a large body; and an HTTP/1.0 request
	// has no chunked framing, so its body is empty. None is forwarded.
	head := "POST /a/large HTTP/1.1\r\nHost: gw\r\nConnection: close\r\nContent-Type: application/json\r\n"
	framingCases := []struct {
		name, request string
		status        int
		reason        string
	}{
		{"invalid chunk size", head + "Transfer-Encoding: chunked\r\n\r\nzz\r\n{}\r\n0\r\n\r\n", 400, "read_failed"},
		{"declared length with a chunked body above the ceiling",
			head + "Content-Length: 10\r\nTransfer-Encoding: chunked\r\n\r\n" + chunkedBody(sizedBody("", global+1), 4096), 413, "too_large"},
		{"HTTP/1.0 chunked body above the ceiling", strings.Replace(head, "HTTP/1.1", "HTTP/1.0", 1) +
			"Transfer-Encoding: chunked\r\n\r\n" + chunkedBody(sizedBody("", global+1), 4096), 400, "malformed_json"},
	}
	for _, tc := range framingCases {
		status, reply := e.rawExchange(tc.request, false)
		_, id := requestID(&http.Response{StatusCode: status}, reply)
		events := e.requestEvents(id, 1)
		if status != tc.status || len(events) != 1 || events[0]["stage"] != "BODY" || events[0]["reason"] != tc.reason {
			t.Errorf("%s: status %d (%s), events %v, want %d with reason %s", tc.name, status, reply, events, tc.status, tc.reason)
		}
	}
	if n := e.a.count(); n != forwarded {
		t.Fatalf("%d receipts after the framing cases, want %d", n, forwarded)
	}

	// Concurrent chunked requests at and one byte above the ceiling, whose
	// length is known only as they are read: each accepted body arrives once
	// and intact, and no refused body arrives.
	e.resetReceivers()
	var mu sync.Mutex
	var accepted []string
	var wg sync.WaitGroup
	for client := range 8 {
		wg.Go(func() {
			for i := range 8 {
				size := global + i%2
				body := sizedBody(fmt.Sprintf("c%d-%d-", client, i), size)
				status, reply := e.sendBody("chunked", "/a/large", body)
				want := http.StatusOK
				if size > global {
					want = http.StatusRequestEntityTooLarge
				}
				if status != want {
					t.Errorf("concurrent chunked %d bytes: status %d (%s), want %d", size, status, reply, want)
				}
				if status == http.StatusOK {
					mu.Lock()
					accepted = append(accepted, body)
					mu.Unlock()
				}
			}
		})
	}
	wg.Wait()
	var received []string
	for _, r := range e.a.receipts() {
		received = append(received, r.Body)
	}
	slices.Sort(accepted)
	slices.Sort(received)
	if len(accepted) != 32 || !slices.Equal(received, accepted) {
		t.Fatalf("concurrent: %d accepted, %d received, equal %v", len(accepted), len(received), slices.Equal(received, accepted))
	}
	e.assertReceipts(32, 0, 0)
}
