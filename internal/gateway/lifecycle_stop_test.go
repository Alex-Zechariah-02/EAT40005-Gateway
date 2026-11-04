// Bounded controller stop and admission end.

package gateway

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// Stop waits a bounded time for an admin request in progress, then closes its
// connection and reports the incomplete shutdown rather than a clean one.
func TestStopIsBoundedByStuckAdminRequest(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.policy(e.a, false), seedOptions{})
	c, _ := e.own(e.controllerConfig())
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", e.admin)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// The handler waits for a body that never arrives, up to the admin read
	// timeout, twice the shutdown wait.
	fmt.Fprintf(conn, "POST /admin/session HTTP/1.1\r\nHost: %s\r\nOrigin: %s\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{",
		e.admin, e.adminOrigin())
	time.Sleep(300 * time.Millisecond)

	started := time.Now()
	err = c.Stop()
	elapsed := time.Since(started)
	if err == nil || !strings.Contains(err.Error(), "admin listener shutdown") {
		t.Fatalf("Stop with a stuck admin request: %v", err)
	}
	if elapsed < adminShutdownWait || elapsed > adminShutdownWait+3*time.Second {
		t.Fatalf("Stop took %s, want about %s", elapsed, adminShutdownWait)
	}
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadAll(conn); errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal("the stuck connection is still open after Stop")
	}
	if again := c.Stop(); again == nil || again.Error() != err.Error() {
		t.Fatalf("repeated Stop: %v, want the first result %v", again, err)
	}
	assertReleased(t, c, e.controllerConfig())
}

// A mutation in progress when Stop is called completes, published and
// audited, and every later one is refused as unavailable; concurrent Stop
// calls share one result. The next owner serves the last committed revision.
func TestStopWaitsForMutations(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.policy(e.a, false), seedOptions{})
	c, _ := e.own(e.controllerConfig())

	type result struct {
		outcome Outcome
		status  int
	}
	results := make(chan result, 1000)
	go func() {
		defer close(results)
		expected := int64(1)
		for i := 0; ; i++ {
			outcome, status := c.Publish(context.Background(), "test", Change{
				ID:       fmt.Sprintf("during-stop-%d", i),
				Expected: expected,
				Policy:   mustJSON(e.policy(e.a, i%2 == 0)),
			})
			results <- result{outcome, status}
			if status != http.StatusOK {
				return
			}
			expected = outcome.Revision
		}
	}()
	waitFor(t, time.Now().Add(10*time.Second), "three publications", func() bool {
		g := c.Current()
		return g != nil && g.Revision >= 4
	})
	var wg sync.WaitGroup
	errs := make([]error, 3)
	for i := range errs {
		wg.Go(func() { errs[i] = c.Stop() })
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}
	}

	committed := int64(1)
	var last result
	for r := range results {
		last = r
		if r.status == http.StatusOK {
			if r.outcome.Outcome != "committed" || r.outcome.Revision != committed+1 {
				t.Fatalf("publication %+v after revision %d", r.outcome, committed)
			}
			committed = r.outcome.Revision
		}
	}
	if last.status != http.StatusServiceUnavailable || last.outcome.Reason != "UNAVAILABLE" {
		t.Fatalf("publication after Stop: %d %+v", last.status, last.outcome)
	}

	again, _ := e.own(e.controllerConfig())
	if g := again.Current(); g == nil || g.Revision != committed {
		t.Fatalf("after restart %+v, want revision %d", g, committed)
	}
	if err := again.Stop(); err != nil {
		t.Fatal(err)
	}
	if n := queryInt(t, e.database, `SELECT count(*) FROM revision`); int64(n) != committed {
		t.Fatalf("%d revisions, want %d", n, committed)
	}
	if n := queryInt(t, e.database, `SELECT count(*) FROM transition WHERE outcome = 'committed' AND audited_at IS NULL`); n != 0 {
		t.Fatalf("%d committed transitions without their audit receipt", n)
	}
}

// The stop begins by ending admission and every change of authority: a
// request that captured its generation earlier completes, while a request
// arriving after, on its kept connection or on another, is refused at
// CAPTURE; a mutation is refused, Start is refused and the controller reports
// it is stopping. The stop that follows writes every event of both requests.
func TestBeginStopEndsAdmission(t *testing.T) {
	e := newEnv(t)
	c, _, f := e.serveEnforcement(e.policy(e.a, true), 0)
	held, proceed := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(proceed) })
	// Registered after the server's cleanup, so it runs first: a failure
	// before the release below cannot leave the held request blocking the
	// server's Close.
	t.Cleanup(release)
	f.setRespond(func(w http.ResponseWriter, r *http.Request) error {
		if r.URL.Path == "/a/held" {
			close(held)
			<-proceed
		}
		io.WriteString(w, "forwarded")
		return nil
	})
	conn, err := net.Dial("tcp", e.dataAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	io.WriteString(conn, "GET /a/held HTTP/1.1\r\nHost: gw\r\n\r\n")
	<-held

	c.beginStop()
	if got := c.admission(); got != stateStopping || c.Current() != nil {
		t.Fatalf("after beginStop: admission %q, generation %v", got, c.Current())
	}
	resp, body := e.exchange(http.MethodGet, "/a/x", "", "")
	_, refused := requestID(resp, body)
	if resp.StatusCode != http.StatusServiceUnavailable || e.eventOf(refused, 1)["stage"] != "CAPTURE" {
		t.Fatalf("request after beginStop: %d %s", resp.StatusCode, body)
	}
	if outcome, status := c.Publish(context.Background(), "test", Change{ID: "after-begin-stop", Expected: 1, Policy: mustJSON(e.policy(e.a, false))}); status != http.StatusServiceUnavailable || outcome.Reason != "UNAVAILABLE" {
		t.Fatalf("publication after beginStop: %d %+v", status, outcome)
	}
	if err := c.Start(); err == nil {
		t.Fatal("Start after beginStop succeeded")
	}

	release()
	reader := bufio.NewReader(conn)
	first, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(first.Body)
	if first.StatusCode != http.StatusOK || string(data) != "forwarded" {
		t.Fatalf("request admitted before beginStop: %d %q", first.StatusCode, data)
	}
	heldID := f.handed()[0].header.Get("X-Request-Id")
	io.WriteString(conn, "GET /a/x HTTP/1.1\r\nHost: gw\r\nConnection: close\r\n\r\n")
	second, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	data, _ = io.ReadAll(second.Body)
	_, later := requestID(second, string(data))
	if second.StatusCode != http.StatusServiceUnavailable || len(f.handed()) != 1 {
		t.Fatalf("request on the kept connection after beginStop: %d %s", second.StatusCode, data)
	}

	if err := c.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	events := e.auditEvents()
	for _, want := range []struct{ kind, id string }{
		{"request.admitted", heldID}, {"request.forwarded", heldID}, {"request.rejected", refused}, {"request.rejected", later},
	} {
		if findEvent(events, want.kind, want.id) == nil {
			t.Errorf("no %s record for %s", want.kind, want.id)
		}
	}
}
