// Integration harness upstream receivers.

package gateway

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
)

// receipt is what an upstream actually received.
type receipt struct {
	Method        string
	Target        string // raw request-target as received
	Path          string // decoded path
	Host          string // Host as received
	RequestID     string
	Generation    string
	Authorization bool
	Headers       http.Header
	Body          string
	Trailer       http.Header // request trailers, known once the body is read
}

// receiver is an owned upstream that records every request it receives.
// handle, when set, may take over the response; it returns true if it did.
type receiver struct {
	name   string
	addr   string
	server *http.Server
	mu     sync.Mutex
	got    []receipt
	handle func(w http.ResponseWriter, r *http.Request) bool
}

func newReceiver(t *testing.T, name string) *receiver {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	rc := &receiver{name: name, addr: listener.Addr().String()}
	rc.server = &http.Server{Handler: http.HandlerFunc(rc.serve)}
	go rc.server.Serve(listener)
	t.Cleanup(func() { rc.server.Close() })
	return rc
}

func (rc *receiver) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	rc.mu.Lock()
	rc.got = append(rc.got, receipt{
		Method:        r.Method,
		Target:        r.RequestURI,
		Path:          r.URL.Path,
		Host:          r.Host,
		RequestID:     r.Header.Get("X-Request-Id"),
		Generation:    r.Header.Get("X-Gateway-Generation"),
		Authorization: r.Header.Get("Authorization") != "",
		Headers:       r.Header.Clone(),
		Body:          string(body),
		Trailer:       r.Trailer.Clone(),
	})
	handle := rc.handle
	rc.mu.Unlock()
	if handle != nil && handle(w, r) {
		return
	}
	fmt.Fprintf(w, "%s ok", rc.name)
}

func (rc *receiver) receipts() []receipt {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return append([]receipt(nil), rc.got...)
}

func (rc *receiver) count() int { return len(rc.receipts()) }

func (rc *receiver) reset() {
	rc.mu.Lock()
	rc.got = nil
	rc.mu.Unlock()
}

// assertReceipts checks every receiver, not only the intended one.
func (e *env) assertReceipts(a, b, c int) {
	e.t.Helper()
	if got := [3]int{e.a.count(), e.b.count(), e.c.count()}; got != [3]int{a, b, c} {
		e.t.Fatalf("receipts A,B,C = %v, want %v", got, [3]int{a, b, c})
	}
}

func (e *env) resetReceivers() {
	e.a.reset()
	e.b.reset()
	e.c.reset()
}
