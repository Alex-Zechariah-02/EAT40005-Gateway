// Stalled upstream responses and exchanges.

package gateway

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/forward"
)

// After the response header, the read-idle bound ends an upstream body that
// stops arriving: the client receives what came and then an aborted
// response, never a later complete or substituted one; the upstream request is
// cancelled; the outcome is an incomplete response. A body that keeps
// arriving more slowly than the bound in total, but never pauses for as long,
// completes; a pause before the header is bounded by the header timeout
// instead. A client that leaves during a response ends the upstream request.
func TestNativeForwardingResponseStalls(t *testing.T) {
	const idle = 500 * time.Millisecond
	timeouts := forwardTimeouts
	timeouts.ReadIdle = idle
	e := newEnv(t)
	_, logs := e.serveNative(e.policy(e.a, false), timeouts)
	var respond func(w http.ResponseWriter, r *http.Request)
	var mu sync.Mutex
	e.a.handle = func(w http.ResponseWriter, r *http.Request) bool {
		mu.Lock()
		f := respond
		mu.Unlock()
		f(w, r)
		return true
	}
	set := func(f func(w http.ResponseWriter, r *http.Request)) {
		mu.Lock()
		respond = f
		mu.Unlock()
	}
	lastOutcome := func() map[string]any {
		got := e.a.receipts()
		return e.outcomeOf(got[len(got)-1].RequestID)
	}

	t.Run("stall after the header", func(t *testing.T) {
		cancelled := make(chan time.Time, 1)
		set(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, "partial ")
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				cancelled <- time.Now()
			case <-time.After(30 * time.Second):
			}
		})
		resp, _ := e.openResponse("GET /a/x HTTP/1.1\r\nHost: gw\r\n\r\n")
		first := make([]byte, len("partial "))
		if _, err := io.ReadFull(resp.Body, first); err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("response %d, first bytes %q: %v", resp.StatusCode, first, err)
		}
		stalled := time.Now()
		rest, err := io.ReadAll(resp.Body)
		ended := time.Since(stalled)
		if err == nil || len(rest) != 0 || ended < idle-100*time.Millisecond || ended > 3*time.Second {
			t.Fatalf("after the stall: %q, %v, after %v (bound %v)", rest, err, ended, idle)
		}
		select {
		case at := <-cancelled:
			t.Logf("client saw the abort %v after the stall began; the upstream request was cancelled %v after it", ended, at.Sub(stalled))
		case <-time.After(5 * time.Second):
			t.Fatal("the stalled upstream request was not cancelled")
		}
		if ev := lastOutcome(); !failedOutcome(ev, http.StatusOK, "upstream_incomplete", "") {
			t.Fatalf("outcome %v", ev)
		}
		if !strings.Contains(logs.String(), forward.ErrResponseStalled.Error()) {
			t.Fatalf("proxy log %s", logs)
		}
	})

	t.Run("slow but progressing", func(t *testing.T) {
		set(func(w http.ResponseWriter, r *http.Request) {
			for i := range 12 {
				fmt.Fprint(w, i%10)
				w.(http.Flusher).Flush()
				time.Sleep(idle * 2 / 5)
			}
		})
		started := time.Now()
		resp, body := e.exchange(http.MethodGet, "/a/x", "", "")
		elapsed := time.Since(started)
		if resp.StatusCode != http.StatusOK || body != "012345678901" || elapsed < 4*idle {
			t.Fatalf("slow response: %d %q after %v", resp.StatusCode, body, elapsed)
		}
		if ev := lastOutcome(); ev["decision"] != "forwarded" || ev["status"] != float64(200) {
			t.Fatalf("outcome %v", ev)
		}
	})

	t.Run("pause before the header", func(t *testing.T) {
		set(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(3 * idle)
			io.WriteString(w, "late")
		})
		if code, body := e.get("/a/x"); code != http.StatusOK || body != "late" {
			t.Fatalf("pause before the header: %d %q", code, body)
		}
	})

	t.Run("partial then closed", func(t *testing.T) {
		// The upstream declares twice the bytes it sends. A response the
		// gateway had only begun buffering never reaches the client; a longer
		// one arrives in part and then the connection closes. Neither ends
		// as a complete response.
		for _, sent := range []int{10, 16 << 10} {
			set(func(w http.ResponseWriter, r *http.Request) {
				conn, buffered, _ := http.NewResponseController(w).Hijack()
				fmt.Fprintf(buffered, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n%s", 2*sent, strings.Repeat("x", sent))
				buffered.Flush()
				conn.Close()
			})
			conn, err := net.DialTimeout("tcp", e.dataAddr, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			conn.SetDeadline(time.Now().Add(10 * time.Second))
			io.WriteString(conn, "GET /a/x HTTP/1.1\r\nHost: gw\r\n\r\n")
			resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				t.Logf("%d of %d bytes sent: the client received no response (%v)", sent, 2*sent, err)
			} else {
				received, err := io.ReadAll(resp.Body)
				if resp.StatusCode != http.StatusOK || !errors.Is(err, io.ErrUnexpectedEOF) || len(received) > sent {
					t.Fatalf("%d of %d bytes sent: client received %d %d bytes, %v", sent, 2*sent, resp.StatusCode, len(received), err)
				}
				t.Logf("%d of %d bytes sent: the client received status %d and %d bytes, then %v", sent, 2*sent, resp.StatusCode, len(received), err)
			}
			conn.Close()
			if ev := lastOutcome(); !failedOutcome(ev, http.StatusOK, "upstream_incomplete", "") {
				t.Fatalf("%d bytes sent: outcome %v", sent, ev)
			}
		}
	})

	t.Run("client leaves during the response", func(t *testing.T) {
		cancelled := make(chan struct{})
		set(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, "first")
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				close(cancelled)
			case <-time.After(30 * time.Second):
			}
		})
		resp, conn := e.openResponse("GET /a/x HTTP/1.1\r\nHost: gw\r\n\r\n")
		first := make([]byte, len("first"))
		if _, err := io.ReadFull(resp.Body, first); err != nil {
			t.Fatal(err)
		}
		conn.Close()
		select {
		case <-cancelled:
		case <-time.After(idle / 2):
			t.Fatal("the upstream request outlived the client")
		}
		if ev := lastOutcome(); !failedOutcome(ev, http.StatusOK, "upstream_incomplete", "") {
			t.Fatalf("outcome %v", ev)
		}
	})
}

// Under load, every stalled or abandoned exchange ends within its bound and
// releases its upstream request and connection: no transport goroutine is
// left once the clients are done.
func TestNativeForwardingReleasesStalledExchanges(t *testing.T) {
	const transportGoroutine = "net/http.(*persistConn)."
	e := newEnv(t)
	timeouts := forwardTimeouts
	timeouts.ReadIdle = 300 * time.Millisecond
	e.serveNative(e.policy(e.a, false), timeouts)
	before := goroutines(transportGoroutine)
	var cancelled atomic.Int64
	e.a.handle = func(w http.ResponseWriter, r *http.Request) bool {
		io.WriteString(w, "partial")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			cancelled.Add(1)
		case <-time.After(30 * time.Second):
		}
		return true
	}
	const stalled, abandoned = 16, 8
	started := time.Now()
	var clients sync.WaitGroup
	for i := range stalled + abandoned {
		clients.Go(func() {
			conn, err := net.DialTimeout("tcp", e.dataAddr, 5*time.Second)
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(20 * time.Second))
			io.WriteString(conn, "GET /a/x HTTP/1.1\r\nHost: gw\r\n\r\n")
			resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				t.Error(err)
				return
			}
			first := make([]byte, len("partial"))
			if _, err := io.ReadFull(resp.Body, first); err != nil {
				t.Error(err)
				return
			}
			if i >= stalled {
				return // the client leaves
			}
			if _, err := io.ReadAll(resp.Body); err == nil {
				t.Error("a stalled response was completed")
			}
		})
	}
	clients.Wait()
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Errorf("stalled exchanges ended after %v", elapsed)
	}
	waitFor(t, time.Now().Add(5*time.Second), "every upstream request to be cancelled", func() bool { return cancelled.Load() == stalled+abandoned })
	waitGoroutines(t, transportGoroutine, before)
	incomplete := 0
	for _, ev := range e.eventsWhen(func(events []map[string]any) bool { return countKind(events, "request.forwarded") == stalled+abandoned }) {
		if ev["kind"] == "request.forwarded" && ev["error"] == "upstream_incomplete" {
			incomplete++
		}
	}
	if incomplete != stalled+abandoned {
		t.Fatalf("%d incomplete outcomes, want %d", incomplete, stalled+abandoned)
	}
	e.assertReceipts(stalled+abandoned, 0, 0)
}
