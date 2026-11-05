// Forwarding failures and no resend.

package gateway

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/forward"
	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

// Failures before any response are answered by the gateway with a bounded,
// safe error and recorded with what is known about delivery: "not_sent" when
// no connection to the upstream existed, "outcome_unknown" when the upstream
// may have acted on the request, and "client_cancelled" when the client went
// away. Nothing is resent and no other destination is tried.
func TestNativeForwardingFailuresBeforeResponse(t *testing.T) {
	t.Run("connection refused", func(t *testing.T) {
		e := newEnv(t)
		closed := freeAddr(t)
		e.serveNative(e.policyWithUpstream(closed), forwardTimeouts)
		resp, body := e.exchange(http.MethodGet, "/z", "", "")
		_, id := requestID(resp, body)
		if resp.StatusCode != http.StatusBadGateway || strings.Contains(body, port(closed)) ||
			!failedOutcome(e.outcomeOf(id), http.StatusBadGateway, "upstream_unavailable", "not_sent") ||
			!sameStages(e.outcomeOf(id)["stages"], publicAdmitted(stageFail)) {
			t.Fatalf("refused upstream: %d %s, outcome %v", resp.StatusCode, body, e.outcomeOf(id))
		}
		e.assertReceipts(0, 0, 0)
	})

	t.Run("upstream that never accepts", func(t *testing.T) {
		e := newEnv(t)
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		timeouts := forwardTimeouts
		timeouts.ResponseHeader = 500 * time.Millisecond
		e.serveNative(e.policyWithUpstream(listener.Addr().String()), timeouts)
		// The connection completes in the listener's queue, so the request
		// may have been delivered as far as the gateway can tell.
		resp, body := e.exchange(http.MethodGet, "/z", "", "")
		_, id := requestID(resp, body)
		if resp.StatusCode != http.StatusGatewayTimeout || !failedOutcome(e.outcomeOf(id), http.StatusGatewayTimeout, "upstream_timeout", "outcome_unknown") {
			t.Fatalf("upstream that never accepts: %d %s, outcome %v", resp.StatusCode, body, e.outcomeOf(id))
		}
	})

	stagedFailures := []struct {
		name   string
		handle func(w http.ResponseWriter, r *http.Request) bool
		status int
		code   string
	}{
		{"closed before the response", func(w http.ResponseWriter, r *http.Request) bool {
			conn, _, _ := http.NewResponseController(w).Hijack()
			conn.Close()
			return true
		}, http.StatusBadGateway, "upstream_unavailable"},
		{"malformed response", func(w http.ResponseWriter, r *http.Request) bool {
			conn, buffered, _ := http.NewResponseController(w).Hijack()
			buffered.WriteString("this is not HTTP\r\n\r\n")
			buffered.Flush()
			conn.Close()
			return true
		}, http.StatusBadGateway, "upstream_unavailable"},
	}
	for _, tc := range stagedFailures {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.serveNative(e.policy(e.a, false), forwardTimeouts)
			e.a.handle = tc.handle
			resp, body := e.exchange(http.MethodGet, "/a/x", "", "")
			_, id := requestID(resp, body)
			if resp.StatusCode != tc.status || !strings.Contains(body, `"error":"`+tc.code+`"`) || strings.Contains(body, "HTTP") ||
				!failedOutcome(e.outcomeOf(id), tc.status, tc.code, "outcome_unknown") {
				t.Fatalf("%d %s, outcome %v", resp.StatusCode, body, e.outcomeOf(id))
			}
			e.assertReceipts(1, 0, 0)
		})
	}

	t.Run("response-header timeout", func(t *testing.T) {
		e := newEnv(t)
		timeouts := forwardTimeouts
		timeouts.ResponseHeader = 500 * time.Millisecond
		e.serveNative(e.policy(e.a, false), timeouts)
		cancelled := make(chan struct{})
		e.a.handle = func(w http.ResponseWriter, r *http.Request) bool {
			select {
			case <-r.Context().Done():
				close(cancelled)
			case <-time.After(10 * time.Second):
			}
			return true
		}
		started := time.Now()
		resp, body := e.exchange(http.MethodGet, "/a/x", "", "")
		elapsed := time.Since(started)
		_, id := requestID(resp, body)
		if resp.StatusCode != http.StatusGatewayTimeout || elapsed > 3*time.Second ||
			!failedOutcome(e.outcomeOf(id), http.StatusGatewayTimeout, "upstream_timeout", "outcome_unknown") {
			t.Fatalf("slow upstream: %d %s after %v, outcome %v", resp.StatusCode, body, elapsed, e.outcomeOf(id))
		}
		select {
		case <-cancelled:
		case <-time.After(5 * time.Second):
			t.Fatal("the upstream request was not cancelled")
		}
		// The request did arrive; the gateway cannot undo what the upstream did.
		e.assertReceipts(1, 0, 0)
	})

	t.Run("client cancels", func(t *testing.T) {
		e := newEnv(t)
		e.serveNative(e.policy(e.a, false), forwardTimeouts)
		received, cancelled := make(chan struct{}), make(chan struct{})
		e.a.handle = func(w http.ResponseWriter, r *http.Request) bool {
			close(received)
			select {
			case <-r.Context().Done():
				close(cancelled)
			case <-time.After(30 * time.Second):
			}
			return true
		}
		conn, err := net.DialTimeout("tcp", e.dataAddr, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(conn, "GET /a/x HTTP/1.1\r\nHost: gw\r\n\r\n")
		<-received
		conn.Close()
		select {
		case <-cancelled:
		case <-time.After(5 * time.Second):
			t.Fatal("the client's cancellation did not reach the upstream")
		}
		ev := e.outcomeOf(e.a.receipts()[0].RequestID)
		if !failedOutcome(ev, http.StatusBadGateway, "upstream_unavailable", "client_cancelled") || !sameStages(ev["stages"], publicAdmitted(stageFail)) {
			t.Fatalf("outcome %v", ev)
		}
		e.assertReceipts(1, 0, 0)
	})
}

// The gateway's answer and the delivery reason for each way an exchange can
// fail before any response, including a dial timeout, which a loopback
// upstream cannot produce on every platform: an unanswered connection
// attempt is refused at once on Windows even when the listener's queue is
// full. The dial errors are the real dialer's.
func TestNativeForwardingFailureClassification(t *testing.T) {
	addr := freeAddr(t)
	_, refused := net.Dial("tcp", addr)
	_, timedOut := (&net.Dialer{Deadline: time.Now().Add(-time.Second)}).Dial("tcp", addr)
	if refused == nil || timedOut == nil {
		t.Fatalf("dial errors: %v, %v", refused, timedOut)
	}
	if netErr, ok := errors.AsType[net.Error](timedOut); !ok || !netErr.Timeout() {
		t.Fatalf("the expired dial did not time out: %v", timedOut)
	}
	headerTimeout := &url.Error{Op: "Get", URL: "http://" + addr, Err: timeoutError{}}
	cases := []struct {
		name                  string
		err                   error
		connected, clientGone bool
		status                int
		code, reason          string
	}{
		{"connection refused", refused, false, false, 502, "upstream_unavailable", "not_sent"},
		{"dial timeout", timedOut, false, false, 502, "upstream_unavailable", "not_sent"},
		{"client left before a connection", context.Canceled, false, true, 502, "upstream_unavailable", "not_sent"},
		{"client left once connected", context.Canceled, true, true, 502, "upstream_unavailable", "client_cancelled"},
		{"connection closed", io.EOF, true, false, 502, "upstream_unavailable", "outcome_unknown"},
		{"response-header timeout", headerTimeout, true, false, 504, "upstream_timeout", "outcome_unknown"},
	}
	for _, tc := range cases {
		err := forward.ExchangeFailure(tc.err, tc.connected, tc.clientGone)
		status, code := forward.ForwardFailure(err)
		if status != tc.status || code != tc.code || forward.FailureReason(err) != tc.reason || !errors.Is(err, tc.err) {
			t.Errorf("%s: %v gives %d %s (%s), want %d %s (%s)", tc.name, err, status, code, forward.FailureReason(err), tc.status, tc.code, tc.reason)
		}
	}
}

// timeoutError is a net.Error that timed out, as the transport's
// response-header timeout error is.
type timeoutError struct{}

func (timeoutError) Error() string   { return "timeout awaiting response headers" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

// A request whose response is lost is sent once only. A write the upstream
// applied before closing the connection is applied once and recorded as of
// unknown outcome; reads and body-less writes carrying an Idempotency-Key,
// which a transport with idle connections would resend after a reused
// connection fails, are not resent either.
func TestNativeForwardingNeverResends(t *testing.T) {
	e := newEnv(t)
	p := e.policy(e.a, false)
	p.Operations = append(p.Operations, policy.Operation{ID: "remove-a", API: "a", Method: "DELETE", Target: "/a/item", Access: "allow", Auth: "none"})
	e.serveNative(p, forwardTimeouts)
	var applied atomic.Int64 // the upstream's own record of writes it carried out
	e.a.handle = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/a/warm" {
			return false
		}
		if r.Method != http.MethodGet {
			applied.Add(1)
		}
		// Applied, then the response is lost.
		conn, _, _ := http.NewResponseController(w).Hijack()
		conn.Close()
		return true
	}
	requests := []struct {
		name, method, path, ctype, body string
		header                          []string
		writes                          int64
	}{
		{"write with a body", http.MethodPost, "/a/item", "application/json", `{"n":1}`, []string{"Idempotency-Key", "write-0001"}, 1},
		{"read", http.MethodGet, "/a/lost", "", "", nil, 0},
		{"body-less write with an idempotency key", http.MethodDelete, "/a/item", "", "", []string{"Idempotency-Key", "delete-0001"}, 1},
	}
	for _, tc := range requests {
		// A completed exchange first: a transport keeping idle connections
		// would send the next request on the same connection.
		if code, body := e.get("/a/warm"); code != http.StatusOK || body != "A ok" {
			t.Fatalf("%s: warm-up %d %q", tc.name, code, body)
		}
		received, writes := e.a.count(), applied.Load()
		resp, body := e.exchange(tc.method, tc.path, tc.ctype, tc.body, tc.header...)
		_, id := requestID(resp, body)
		if resp.StatusCode != http.StatusBadGateway || !failedOutcome(e.outcomeOf(id), http.StatusBadGateway, "upstream_unavailable", "outcome_unknown") {
			t.Errorf("%s: %d %s, outcome %v", tc.name, resp.StatusCode, body, e.outcomeOf(id))
		}
		if got, done := e.a.count()-received, applied.Load()-writes; got != 1 || done != tc.writes {
			t.Errorf("%s: the upstream received it %d times and applied %d writes, want once and %d", tc.name, got, done, tc.writes)
		}
	}
}
