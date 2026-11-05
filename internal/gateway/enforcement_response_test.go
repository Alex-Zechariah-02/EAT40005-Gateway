// Enforcement responses and trusted binding.

package gateway

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/Alex-Zechariah-02/apiward/internal/forward"
)

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
		{"upstream timed out", fmt.Errorf("waiting for headers: %w", forward.ErrUpstreamTimeout), http.StatusGatewayTimeout, "upstream_timeout"},
		{"no upstream", fmt.Errorf("source failed: %w", forward.ErrNoUpstream), http.StatusServiceUnavailable, "gateway_unavailable"},
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
