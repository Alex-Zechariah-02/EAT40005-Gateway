// Bound upstreams, publication and captured destinations.

package gateway

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAdmittedRequestReachesOnlyBoundUpstream(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.policy(e.a, false), seedOptions{})
	e.start(binaries.plain)
	g := e.status()

	if code, body := e.get("/a/x"); code != http.StatusOK || body != "A ok" {
		t.Fatalf("admitted request: %d %q", code, body)
	}
	e.assertReceipts(1, 0, 0)
	if got := e.a.receipts()[0]; got.Generation != g.Generation || got.Target != "/a/x" {
		t.Fatalf("receipt %+v, want generation %s", got, g.Generation)
	}

	if code, _ := e.get("/d"); code != http.StatusForbidden {
		t.Fatalf("denied operation: %d", code)
	}
	if code, _ := e.get("/zzz"); code != http.StatusNotFound {
		t.Fatalf("unknown operation: %d", code)
	}
	e.assertReceipts(1, 0, 0)

	events := e.eventsWhen(func(events []map[string]any) bool {
		return countKind(events, "request.rejected") >= 2 && countKind(events, "request.admitted") >= 1
	})
	admitted, rejected := countKind(events, "request.admitted"), countKind(events, "request.rejected")
	if admitted != 1 || rejected != 2 {
		t.Fatalf("audit admitted=%d rejected=%d, want 1 and 2", admitted, rejected)
	}
}

func TestPublicationAddsOperationWithoutReload(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.policy(e.a, false), seedOptions{})
	configHash := fileSHA256(t, e.config)
	e.start(binaries.fault)
	pid := e.process.Process.Pid

	if code, _ := e.get("/b/x"); code != http.StatusNotFound {
		t.Fatalf("before publication: %d", code)
	}
	outcome, code := e.publish(Change{ID: "add-b", Expected: 1, Policy: mustJSON(e.policy(e.a, true))})
	if code != http.StatusOK || outcome.Revision != 2 {
		t.Fatalf("publish: %d %+v", code, outcome)
	}
	if code, body := e.get("/b/x"); code != http.StatusOK || body != "B ok" {
		t.Fatalf("new operation: %d %q", code, body)
	}
	if code, body := e.get("/a/x"); code != http.StatusOK || body != "A ok" {
		t.Fatalf("existing operation: %d %q", code, body)
	}
	e.assertReceipts(1, 1, 0)
	for _, r := range append(e.a.receipts(), e.b.receipts()...) {
		if r.Generation != outcome.Generation {
			t.Fatalf("receipt generation %s, want %s", r.Generation, outcome.Generation)
		}
	}
	if e.process.Process.Pid != pid || fileSHA256(t, e.config) != configHash {
		t.Fatal("gateway process or configuration changed")
	}
}

// holdRequest sends the headers and half of a body for POST /a/item, then
// waits until the gateway signals that the request captured its generation.
// A non-empty token is sent as the bearer credential.
func holdRequest(t *testing.T, e *env, holdFile, token string) (net.Conn, string) {
	t.Helper()
	conn, err := net.Dial("tcp", e.dataAddr)
	if err != nil {
		t.Fatal(err)
	}
	credential := ""
	if token != "" {
		credential = "Authorization: Bearer " + token + "\r\n"
	}
	fmt.Fprintf(conn, "POST /a/item HTTP/1.1\r\nHost: gw\r\n%sContent-Type: application/json\r\nContent-Length: 13\r\nConnection: close\r\n\r\n{\"half\":", credential)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(holdFile); err == nil && len(data) > 0 {
			return conn, strings.TrimSpace(string(data))
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("request never captured a generation")
	return nil, ""
}

func TestInFlightRequestKeepsCapturedGeneration(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.policy(e.a, false), seedOptions{})
	holdFile := filepath.Join(e.dir, "hold")
	e.start(binaries.fault, "GATEWAY_HOLD_PATH=/a/item", "GATEWAY_HOLD_FILE="+holdFile)
	first := e.status().Generation

	conn, captured := holdRequest(t, e, holdFile, "")
	defer conn.Close()
	if captured != first {
		t.Fatalf("captured %s, want %s", captured, first)
	}
	outcome, code := e.publish(Change{ID: "move-a", Expected: 1, Policy: mustJSON(e.policy(e.b, false))})
	if code != http.StatusOK {
		t.Fatalf("publish: %d %+v", code, outcome)
	}

	io.WriteString(conn, "true}")
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("in-flight request: %v %v", resp, err)
	}
	e.assertReceipts(1, 0, 0)
	if got := e.a.receipts()[0].Generation; got != first {
		t.Fatalf("in-flight request reached A under %s, want %s", got, first)
	}

	if code, body := e.request(http.MethodPost, "/a/item", "application/json", `{"n":1}`); code != http.StatusOK || body != "B ok" {
		t.Fatalf("new request: %d %q", code, body)
	}
	e.assertReceipts(1, 1, 0)
	if got := e.b.receipts()[0].Generation; got != outcome.Generation {
		t.Fatalf("new request reached B under %s, want %s", got, outcome.Generation)
	}

	// The events agree with the receipts: each request was matched, bound
	// and recorded under the generation it captured.
	held, later := e.a.receipts()[0].RequestID, e.b.receipts()[0].RequestID
	events := e.eventsWhen(func(events []map[string]any) bool { return countKind(events, "request.forwarded") == 2 })
	for _, want := range []struct{ id, generation, upstream string }{{held, first, "A"}, {later, outcome.Generation, "B"}} {
		ev := findEvent(events, "request.forwarded", want.id)
		if ev == nil || ev["generation"] != want.generation || ev["upstream"] != want.upstream || ev["operation"] != "write-a" {
			t.Fatalf("event %v, want generation %s upstream %s", ev, want.generation, want.upstream)
		}
	}
}

// A request whose exchange fails while a publication moves its operation to
// another upstream is answered 502 and was sent once, to the destination of
// the generation it captured; it is never sent again, there or elsewhere.
func TestFailedAttemptStaysOnCapturedDestination(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.policy(e.a, false), seedOptions{})
	e.start(binaries.fault)
	captured := e.status().Generation

	published := make(chan struct{})
	var once sync.Once
	e.a.handle = func(w http.ResponseWriter, r *http.Request) bool {
		once.Do(func() {
			outcome, code := e.publish(Change{ID: "move-a", Expected: 1, Policy: mustJSON(e.policy(e.b, false))})
			if code != http.StatusOK {
				t.Errorf("publish during attempt: %d %+v", code, outcome)
			}
			close(published)
		})
		// Drop the connection without a response: a transport error after
		// the request was sent.
		conn, _, _ := http.NewResponseController(w).Hijack()
		conn.Close()
		return true
	}
	code, _ := e.get("/a/x")
	<-published
	if code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", code)
	}
	e.assertReceipts(1, 0, 0)
	if g := e.a.receipts()[0].Generation; g != captured {
		t.Fatalf("attempt under generation %s, want %s", g, captured)
	}
}
