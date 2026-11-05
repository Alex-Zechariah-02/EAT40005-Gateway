// Native startup failures, release and graceful stop.

package gateway

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/store"
)

// A resource the gateway cannot acquire stops its startup with status 1 and a
// message naming it, and whatever it acquired is released again: no listener
// stays open and the database is free for its next owner. A database that
// another gateway holds is never shared.
func TestNativeStartupResourceFailures(t *testing.T) {
	occupy := func(t *testing.T, addr string) {
		l, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { l.Close() })
	}
	cases := []struct {
		name    string
		prepare func(t *testing.T, e *env) []string // returns the gateway's addresses that must stay closed
		cause   string
	}{
		{"data address in use", func(t *testing.T, e *env) []string { occupy(t, e.dataAddr); return []string{e.admin} }, "data listener"},
		{"admin address in use", func(t *testing.T, e *env) []string { occupy(t, e.admin); return []string{e.dataAddr} }, "admin listener"},
		{"audit log cannot be opened", func(t *testing.T, e *env) []string {
			if err := os.Mkdir(e.auditLog, 0o700); err != nil {
				t.Fatal(err)
			}
			return []string{e.dataAddr, e.admin}
		}, "audit log"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			seed(t, e.database, e.policy(e.a, false), seedOptions{})
			closed := tc.prepare(t, e)
			e.run(binaries.plain)
			if code := e.waitExit(); code != exitFailed || !strings.Contains(e.output.String(), tc.cause) {
				t.Fatalf("exit %d, want %d with %q\n%s", code, exitFailed, tc.cause, e.output)
			}
			for _, addr := range closed {
				if dialable(addr) {
					t.Fatalf("%s is open after the failed start", addr)
				}
			}
			st, err := store.OpenStore(e.database)
			if err != nil {
				t.Fatalf("the database is held: %v", err)
			}
			st.Close()
		})
	}

	t.Run("database held by another gateway", func(t *testing.T) {
		e := newEnv(t)
		seed(t, e.database, e.policy(e.a, false), seedOptions{})
		e.start(binaries.plain)
		generation := e.status().Generation

		second := newEnv(t)
		settings := second.nativeSettings()
		section(settings, "gateway")["database"] = e.database
		second.writeNative(settings)
		second.run(binaries.plain)
		if code := second.waitExit(); code != exitFailed || !strings.Contains(second.output.String(), "held by another gateway process") {
			t.Fatalf("second gateway beside the first: exit %d\n%s", code, second.output)
		}
		if dialable(second.dataAddr) || dialable(second.admin) {
			t.Fatal("the refused gateway opened a listener")
		}
		if _, err := os.Stat(second.auditLog); !os.IsNotExist(err) {
			t.Fatalf("the refused gateway opened an audit log: %v", err)
		}
		if s := e.status(); s.Generation != generation {
			t.Fatalf("the first gateway changed: %+v", s)
		}

		// Once the first gateway has stopped, the second owns the deployment,
		// and the first is refused in its turn.
		e.stop()
		second.start(binaries.plain)
		if s := second.status(); s.Generation != generation || s.Admission != "ready" {
			t.Fatalf("second owner %+v, want generation %s", s, generation)
		}
		e.run(binaries.plain)
		if code := e.waitExit(); code != exitFailed || !strings.Contains(e.output.String(), "held by another gateway process") {
			t.Fatalf("first gateway beside the second: exit %d\n%s", code, e.output)
		}
		if code, body := second.get("/a/x"); code != http.StatusOK || body != "A ok" {
			t.Fatalf("second gateway after the refused start: %d %q", code, body)
		}
	})
}

// The native gateway owned in this process releases everything it acquired:
// after a stop, and after a start that fails part way. Concurrent stops share
// one result.
func TestNativeStartAndStopRelease(t *testing.T) {
	const dataServer = "gateway.startNative.func"
	e := newEnv(t)
	provisional := e.policy(e.a, true)
	seed(t, e.database, e.policy(e.a, false), seedOptions{provisional: &provisional, deadline: time.Now().Add(time.Hour)})
	e.writeNative(e.nativeSettings())
	cfg, err := loadNativeConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewJSONHandler(&logBuffer{}, nil))
	writers, admins, servers := goroutines(auditWriter), goroutines(adminServer), goroutines(dataServer)

	g, err := startNative(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	if code, body := e.get("/b/x"); code != http.StatusOK || body != "B ok" {
		t.Fatalf("provisional generation: %d %q", code, body)
	}
	errs := make([]error, 3)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Go(func() { errs[i] = g.stop() })
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("stop: %v", err)
		}
	}
	if g.controller.timer.Stop() {
		t.Fatal("the deadline timer was still armed after the stop")
	}
	if err := <-g.served; !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("data-plane Serve returned %v", err)
	}
	assertNativeReleased(t, e, writers, admins, servers)

	for name, addr := range map[string]string{"data": e.dataAddr, "admin": e.admin} {
		l, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		if g, err := startNative(cfg, logger); err == nil || !strings.Contains(err.Error(), name+" listener") {
			if g != nil {
				g.stop()
			}
			t.Fatalf("start with the %s address in use: %v", name, err)
		}
		l.Close()
		assertNativeReleased(t, e, writers, admins, servers)
	}
}

// assertNativeReleased fails unless no native gateway of e's deployment holds
// anything: its goroutines ended, its listeners closed and its database lock
// free.
func assertNativeReleased(t *testing.T, e *env, writers, admins, servers int) {
	t.Helper()
	waitGoroutines(t, auditWriter, writers)
	waitGoroutines(t, adminServer, admins)
	waitGoroutines(t, "gateway.startNative.func", servers)
	if dialable(e.dataAddr) || dialable(e.admin) || dialable(e.metrics) {
		t.Fatal("a listener is still open")
	}
	st, err := store.OpenStore(e.database)
	if err != nil {
		t.Fatalf("the database is still held: %v", err)
	}
	st.Close()
}

// The native process stops gracefully on Ctrl+Break (os.Interrupt): from the
// signal on no new request is admitted and the protected status says it is
// stopping, while a request admitted before finishes; the listeners close,
// the process exits with status 0 and the next start serves the same
// generation. A second signal forces the exit with status 2.
func TestNativeGracefulStop(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.policy(e.a, false), seedOptions{})
	e.interruptible = true
	holdFile, release := filepath.Join(e.dir, "hold"), filepath.Join(e.dir, "release")
	e.start(binaries.fault, "GATEWAY_HOLD_PATH=/a/held", "GATEWAY_HOLD_FILE="+holdFile, "GATEWAY_HOLD_RELEASE="+release)
	generation := e.status().Generation

	idle, err := net.DialTimeout("tcp", e.dataAddr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	io.WriteString(idle, "GET /a/x HTTP/1.1\r\nHost: gw\r\n\r\n")
	idleReader := bufio.NewReader(idle)
	if resp, err := http.ReadResponse(idleReader, nil); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("request on the kept connection: %v", err)
	} else {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	held := make(chan string, 1)
	go func() {
		resp, err := (&http.Client{Timeout: 30 * time.Second}).Get("http://" + e.dataAddr + "/a/held")
		if err != nil {
			held <- err.Error()
			return
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		held <- fmt.Sprintf("%d %s", resp.StatusCode, body)
	}()
	waitForFile(t, holdFile)

	interrupt(t, e.process)
	e.waitOutput(`"msg":"stopping","signal":"interrupt"`)
	waitFor(t, time.Now().Add(5*time.Second), "the data listener to close", func() bool { return !dialable(e.dataAddr) })
	if s := e.status(); s.Admission != stateStopping || s.State != stateStopping {
		t.Fatalf("status while stopping %+v", s)
	}
	if health := e.health(); health != "unavailable" {
		t.Fatalf("health while stopping %q", health)
	}
	idle.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := idleReader.ReadByte(); !errors.Is(err, io.EOF) {
		t.Fatalf("the idle connection was not closed: %v", err)
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := <-held; got != "200 A ok" {
		t.Fatalf("request admitted before the stop: %s", got)
	}
	if code := e.waitExit(); code != exitStopped {
		t.Fatalf("exit status %d\n%s", code, e.output)
	}
	if !strings.Contains(e.output.String(), `"msg":"stopped"`) || dialable(e.admin) {
		t.Fatalf("stop not completed\n%s", e.output)
	}
	e.assertReceipts(2, 0, 0)

	e.interruptible = false
	e.start(binaries.plain)
	if s := e.status(); s.Generation != generation {
		t.Fatalf("generation %s after the restart, want %s", s.Generation, generation)
	}
	if countKind(e.auditEvents(), "audit.recovered") != 0 {
		t.Fatal("the restart found an incomplete audit record")
	}

	t.Run("second signal", func(t *testing.T) {
		e := newEnv(t)
		seed(t, e.database, e.policy(e.a, false), seedOptions{})
		e.interruptible = true
		holdFile, release := filepath.Join(e.dir, "hold"), filepath.Join(e.dir, "release")
		e.start(binaries.fault, "GATEWAY_HOLD_PATH=/a/held", "GATEWAY_HOLD_FILE="+holdFile, "GATEWAY_HOLD_RELEASE="+release)
		go (&http.Client{Timeout: 30 * time.Second}).Get("http://" + e.dataAddr + "/a/held")
		waitForFile(t, holdFile)
		interrupt(t, e.process)
		e.waitOutput(`"msg":"stopping"`)
		interrupt(t, e.process)
		if code := e.waitExit(); code != exitForced || !strings.Contains(e.output.String(), "forced exit") {
			t.Fatalf("exit status %d after two signals\n%s", code, e.output)
		}
		if strings.Contains(e.output.String(), `"msg":"stopped"`) {
			t.Fatal("a forced exit was reported as a completed stop")
		}
	})
}
