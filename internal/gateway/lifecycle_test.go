package gateway

import (
	"bufio"
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests own the controller in the test process, as the gateway
// application does, without a gateway process. They check
// what each lifecycle step leaves behind through the deployment itself: its
// admin listener, database lock, audit file, timers and goroutines.

// logBuffer collects a controller's log output.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// controllerConfig is the configuration of e's deployment.
func (e *env) controllerConfig() Config {
	return Config{
		Database:    e.database,
		AuditLog:    e.auditLog,
		AdminListen: e.admin,
		JWT:         testTrust(testIssuerName, testAudience, e.publicKey),
	}
}

// own creates a controller for cfg in the test process and stops it when the
// test ends.
func (e *env) own(cfg Config) (*Controller, *logBuffer) {
	e.t.Helper()
	logs := &logBuffer{}
	c, err := NewController(cfg, slog.New(slog.NewJSONHandler(logs, nil)))
	if err != nil {
		e.t.Fatalf("NewController: %v", err)
	}
	e.t.Cleanup(func() { c.Stop() })
	return c, logs
}

// Stack frames of the goroutines a controller starts.
const (
	auditWriter = "gateway.(*auditLog).run("
	adminServer = "gateway.(*Controller).startAdmin."
)

// goroutines counts the goroutines whose stack holds frame.
func goroutines(frame string) int {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	count := 0
	for stack := range strings.SplitSeq(string(buf), "\n\n") {
		if strings.Contains(stack, frame) {
			count++
		}
	}
	return count
}

// waitGoroutines waits until at most want goroutines hold frame. A goroutine
// told to stop may still be returning when the call that stopped it returns.
func waitGoroutines(t *testing.T, frame string, want int) {
	t.Helper()
	waitFor(t, time.Now().Add(5*time.Second), fmt.Sprintf("at most %d goroutines in %s", want, frame), func() bool {
		return goroutines(frame) <= want
	})
}

// assertReleased fails unless the stopped controller c holds nothing of its
// deployment: no admin listener, no open database or audit file, and the
// database lock is free for another owner.
func assertReleased(t *testing.T, c *Controller, cfg Config) {
	t.Helper()
	if dialable(cfg.AdminListen) {
		t.Fatal("the admin listener is still open")
	}
	if c.store != nil && c.store.db.Ping() == nil {
		t.Fatal("the database is still open")
	}
	if c.audit.file != nil && !errors.Is(c.audit.file.Close(), os.ErrClosed) {
		t.Fatal("the audit file is still open")
	}
	if _, err := os.Stat(cfg.Database); err == nil {
		st, err := openStore(cfg.Database)
		if err != nil && !errors.Is(err, errUnavailable) {
			t.Fatalf("the database lock is still held: %v", err)
		}
		if st != nil {
			st.close()
		}
	}
}

// An application owns the controller: the policy is published before any
// listener opens, Start serves the Admin API once, and Stop releases
// everything, once, however often it is called.
func TestControllerOwnedByApplication(t *testing.T) {
	e := newEnv(t)
	policy := e.protectedPolicy()
	seed(t, e.database, policy, seedOptions{})
	writers, servers := goroutines(auditWriter), goroutines(adminServer)

	c, logs := e.own(e.controllerConfig())
	want, err := effectiveGeneration(policy, 1, time.Time{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	g := c.Current()
	if g == nil || g.ID != want.ID || g.Revision != 1 {
		t.Fatalf("published %+v, want the canonical compilation of revision 1 (%s)", g, want.ID)
	}
	if dialable(e.admin) {
		t.Fatal("the admin listener is open before Start")
	}
	if got := goroutines(auditWriter); got != writers+1 {
		t.Fatalf("%d audit writers, want %d", got, writers+1)
	}

	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	if health := e.health(); health != modeNormal {
		t.Fatalf("health %q", health)
	}
	if s := e.status(); s.State != modeNormal || s.Generation != g.ID || s.Confirmed != 1 {
		t.Fatalf("status %+v", s)
	}
	if err := c.Start(); err == nil || !strings.Contains(err.Error(), "already started") {
		t.Fatalf("second Start: %v", err)
	}
	if got := goroutines(adminServer); got != servers+1 {
		t.Fatalf("%d admin servers, want %d", got, servers+1)
	}

	if err := c.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := c.Stop(); err != nil {
		t.Fatalf("repeated Stop: %v", err)
	}
	if err := c.Start(); err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("Start after Stop: %v", err)
	}
	if c.Current() != nil {
		t.Fatal("a stopped controller still publishes a generation")
	}
	assertReleased(t, c, e.controllerConfig())
	waitGoroutines(t, auditWriter, writers)
	waitGoroutines(t, adminServer, servers)
	if strings.Contains(logs.String(), `"level":"ERROR"`) {
		t.Fatalf("errors logged:\n%s", logs)
	}

	// Another owner of the deployment serves the same committed authority.
	again, _ := e.own(e.controllerConfig())
	if g2 := again.Current(); g2 == nil || g2.ID != g.ID {
		t.Fatalf("after restart %+v, want %s", g2, g.ID)
	}
	reconciled := func(events []map[string]any) bool { return countKind(events, "controller.reconciled") == 2 }
	if n := countKind(e.eventsWhen(reconciled), "controller.reconciled"); n != 2 {
		t.Fatalf("%d reconciliation events, want 2", n)
	}
}

// Invalid configuration is refused before anything of the deployment is
// opened: no audit file is created, no database lock taken, no listener or
// goroutine started.
func TestControllerRefusesInvalidConfiguration(t *testing.T) {
	privateKey := filepath.Join(t.TempDir(), "private.pem")
	der, _ := x509.MarshalPKCS8PrivateKey(newTestIssuer(2048).key)
	if err := os.WriteFile(privateKey, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		change func(e *env, cfg *Config)
		cause  string
	}{
		{"no database", func(e *env, cfg *Config) { cfg.Database = "" }, "database, audit_log and admin_listen are required"},
		{"no audit log", func(e *env, cfg *Config) { cfg.AuditLog = "" }, "database, audit_log and admin_listen are required"},
		{"no admin listener", func(e *env, cfg *Config) { cfg.AdminListen = "" }, "database, audit_log and admin_listen are required"},
		{"wildcard admin listener", func(e *env, cfg *Config) { cfg.AdminListen = "0.0.0.0:" + port(e.admin) }, "loopback"},
		{"host name admin listener", func(e *env, cfg *Config) { cfg.AdminListen = "localhost:" + port(e.admin) }, "loopback"},
		{"admin listener without a port", func(e *env, cfg *Config) { cfg.AdminListen = "127.0.0.1:0" }, "loopback"},
		{"no trust configuration", func(e *env, cfg *Config) { cfg.JWT = TrustConfig{} }, "jwt: issuer and audience"},
		{"missing key file", func(e *env, cfg *Config) { cfg.JWT.Keys[0].PublicKeyFile = filepath.Join(e.dir, "absent.pem") }, "jwt: opening public key"},
		{"private key given as the key", func(e *env, cfg *Config) { cfg.JWT.Keys[0].PublicKeyFile = privateKey }, "exactly one PEM PUBLIC KEY block"},
		{"audit file size below the bound", func(e *env, cfg *Config) { cfg.AuditMaxBytes = 1024 }, "audit_max_bytes must be"},
		{"negative audit file size", func(e *env, cfg *Config) { cfg.AuditMaxBytes = -1 }, "audit_max_bytes must be"},
		{"one audit file", func(e *env, cfg *Config) { cfg.AuditMaxFiles = 1 }, "audit_max_files must be"},
		{"idle timeout too short", func(e *env, cfg *Config) { cfg.AdminIdleTimeout = 500 * time.Millisecond }, "admin_idle_timeout"},
		{"session lifetime too long", func(e *env, cfg *Config) { cfg.AdminSessionLifetime = 25 * time.Hour }, "admin_session_lifetime"},
		{"dashboard directory without index.html", func(e *env, cfg *Config) { cfg.AdminUI = t.TempDir() }, "admin_ui"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			seed(t, e.database, e.policy(e.a, false), seedOptions{})
			cfg := e.controllerConfig()
			tc.change(e, &cfg)
			writers := goroutines(auditWriter)
			c, err := NewController(cfg, slog.New(slog.DiscardHandler))
			if c != nil || err == nil || !strings.Contains(err.Error(), tc.cause) {
				t.Fatalf("NewController = %v, %v; want an error with %q", c, err, tc.cause)
			}
			if strings.Contains(err.Error(), "PRIVATE KEY") || strings.Contains(err.Error(), "MII") {
				t.Fatalf("the error discloses key material: %v", err)
			}
			if _, err := os.Stat(e.auditLog); !os.IsNotExist(err) {
				t.Fatalf("an audit file was created: %v", err)
			}
			if got := goroutines(auditWriter); got != writers {
				t.Fatalf("%d audit writers, want %d", got, writers)
			}
			if dialable(e.admin) {
				t.Fatal("a listener is open")
			}
			st, err := openStore(e.database)
			if err != nil {
				t.Fatalf("the database is held: %v", err)
			}
			st.close()
		})
	}

	t.Run("no logger", func(t *testing.T) {
		e := newEnv(t)
		seed(t, e.database, e.policy(e.a, false), seedOptions{})
		if c, err := NewController(e.controllerConfig(), nil); c != nil || err == nil {
			t.Fatalf("NewController without a logger = %v, %v", c, err)
		}
		if _, err := os.Stat(e.auditLog); !os.IsNotExist(err) {
			t.Fatalf("an audit file was created: %v", err)
		}
	})
}

// A deployment without usable authority, including a stored policy the
// compiler refuses, gives a controller that serves nothing and says why; it
// never serves an earlier or default policy, and never creates a database.
func TestControllerWithoutAuthorityServesNothing(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(e *env)
		reason  string
	}{
		{"missing database", func(e *env) {}, "missing or empty"},
		{"earlier schema version", func(e *env) {
			seed(e.t, e.database, e.policy(e.a, false), seedOptions{version: databaseVersion - 1})
		}, "run its migrate command"},
		{"stored revision altered", func(e *env) {
			seed(e.t, e.database, e.policy(e.a, false), seedOptions{})
			db := inspect(e.t, e.database)
			if _, err := db.Exec(`UPDATE revision SET document = ? WHERE id = 1`, []byte(`{}`)); err != nil {
				e.t.Fatal(err)
			}
			db.Close()
		}, "checksum mismatch"},
		{"stored policy refused by the compiler", func(e *env) {
			seed(e.t, e.database, e.policy(e.a, false), seedOptions{})
			earlier := []byte(`{"schema_version":1,"profile":"routing-only","upstreams":[{"id":"A","dial":"` + e.a.addr + `"}],` +
				`"apis":[{"id":"a","upstream":"A"}],` +
				`"operations":[{"id":"read-a","api":"a","method":"GET","target":"/a/*","access":"allow","auth":"required"}]}`)
			db := inspect(e.t, e.database)
			if _, err := db.Exec(`UPDATE revision SET document = ?, checksum = ? WHERE id = 1`, earlier, documentChecksum(earlier)); err != nil {
				e.t.Fatal(err)
			}
			db.Close()
		}, "SCHEMA_VERSION_UNSUPPORTED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			tc.prepare(e)
			c, _ := e.own(e.controllerConfig())
			if g := c.Current(); g != nil {
				t.Fatalf("published %s without usable authority", g.ID)
			}
			if err := c.Start(); err != nil {
				t.Fatal(err)
			}
			if health := e.health(); health != "unavailable" {
				t.Fatalf("health %q", health)
			}
			if reason := e.unavailableReason(); !strings.Contains(reason, tc.reason) {
				t.Fatalf("recorded reason %q, want %q", reason, tc.reason)
			}
			if err := c.Stop(); err != nil {
				t.Fatal(err)
			}
			assertReleased(t, c, e.controllerConfig())
			if tc.name == "missing database" {
				if _, err := os.Stat(e.database); !os.IsNotExist(err) {
					t.Fatalf("a database was created: %v", err)
				}
			}
		})
	}
}

// One deployment has one controller: a second one is refused while the first
// holds the database, before it opens the audit log or a listener, and the
// first keeps serving. Once the first stops, the second can take over.
func TestSecondControllerCannotOwnDeployment(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.policy(e.a, false), seedOptions{})
	first, _ := e.own(e.controllerConfig())
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	g := first.Current()

	second := e.controllerConfig()
	second.AuditLog = filepath.Join(e.dir, "second-audit.log")
	second.AdminListen = freeAddr(t)
	writers := goroutines(auditWriter)
	c, err := NewController(second, slog.New(slog.DiscardHandler))
	if c != nil || err == nil || !strings.Contains(err.Error(), "held by another gateway process") {
		t.Fatalf("second controller = %v, %v", c, err)
	}
	if _, err := os.Stat(second.AuditLog); !os.IsNotExist(err) {
		t.Fatalf("the refused controller opened an audit log: %v", err)
	}
	if got := goroutines(auditWriter); got != writers {
		t.Fatalf("%d audit writers, want %d", got, writers)
	}
	if dialable(second.AdminListen) {
		t.Fatal("the refused controller opened a listener")
	}
	if first.Current() != g || e.health() != modeNormal {
		t.Fatal("the first controller changed")
	}

	if err := first.Stop(); err != nil {
		t.Fatal(err)
	}
	takeover, _ := e.own(second)
	if got := takeover.Current(); got == nil || got.ID != g.ID {
		t.Fatalf("after takeover %+v, want %s", got, g.ID)
	}
}

// A Start that cannot open the admin listener leaves no listener or server
// goroutine, and Stop still releases the database and audit log.
func TestStartFailureLeavesNoListener(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.policy(e.a, false), seedOptions{})
	occupied, err := net.Listen("tcp", e.admin)
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	writers, servers := goroutines(auditWriter), goroutines(adminServer)

	c, _ := e.own(e.controllerConfig())
	if err := c.Start(); err == nil {
		t.Fatal("Start succeeded on an occupied address")
	}
	if got := goroutines(adminServer); got != servers {
		t.Fatalf("%d admin servers, want %d", got, servers)
	}
	if err := c.Stop(); err != nil {
		t.Fatalf("Stop after the failed start: %v", err)
	}
	occupied.Close()
	assertReleased(t, c, e.controllerConfig())
	waitGoroutines(t, auditWriter, writers)
}

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

// Stop ends the deadline timer: a provisional base whose deadline passes
// after the controller stopped is not rolled back by it, and the next owner
// rolls it back at startup, once.
func TestStopEndsDeadlineTimer(t *testing.T) {
	e := newEnv(t)
	provisional := e.policy(e.a, true)
	deadline := time.UnixMilli(time.Now().Add(1500 * time.Millisecond).UnixMilli())
	seed(t, e.database, e.policy(e.a, false), seedOptions{provisional: &provisional, deadline: deadline})

	c, _ := e.own(e.controllerConfig())
	if g := c.Current(); g == nil || g.Revision != 2 || !g.NotAfter.Equal(deadline) {
		t.Fatalf("published %+v, want provisional revision 2 until %s", g, deadline)
	}
	if err := c.Stop(); err != nil {
		t.Fatal(err)
	}
	if c.timer == nil || c.timer.Stop() {
		t.Fatal("the deadline timer was still armed after Stop")
	}
	time.Sleep(time.Until(deadline.Add(700 * time.Millisecond)))
	if n := queryInt(t, e.database, `SELECT coalesce(provisional, 0) FROM base`); n != 2 {
		t.Fatalf("provisional %d after the deadline passed while stopped, want 2", n)
	}
	if n := queryInt(t, e.database, `SELECT count(*) FROM transition WHERE id LIKE 'auto-rollback:%'`); n != 0 {
		t.Fatalf("%d rollbacks recorded by a stopped controller", n)
	}

	again, _ := e.own(e.controllerConfig())
	if g := again.Current(); g == nil || g.Revision != 1 || !g.NotAfter.IsZero() {
		t.Fatalf("after restart %+v, want confirmed revision 1", g)
	}
	if err := again.Stop(); err != nil {
		t.Fatal(err)
	}
	if n := queryInt(t, e.database, `SELECT count(*) FROM transition WHERE id = 'auto-rollback:2' AND outcome = 'committed' AND audited_at IS NOT NULL`); n != 1 {
		t.Fatalf("%d audited rollbacks, want 1", n)
	}
}

// Stop racing the deadline timer leaves the rollback either complete,
// published and audited, or not started; never half done. The next owner
// always ends with the rollback applied exactly once.
func TestStopRacesDeadline(t *testing.T) {
	outcomes := map[string]int{}
	for round := range 10 {
		e := newEnv(t)
		provisional := e.policy(e.a, true)
		deadline := time.UnixMilli(time.Now().Add(400 * time.Millisecond).UnixMilli())
		seed(t, e.database, e.policy(e.a, false), seedOptions{provisional: &provisional, deadline: deadline})
		c, _ := e.own(e.controllerConfig())

		// Stop lands from a few milliseconds before to a few after the timer.
		time.Sleep(time.Until(deadline.Add(time.Duration(round-5) * time.Millisecond)))
		var wg sync.WaitGroup
		errs := make([]error, 3)
		for i := range errs {
			wg.Go(func() { errs[i] = c.Stop() })
		}
		wg.Wait()
		for _, err := range errs {
			if err != nil {
				t.Fatalf("round %d: Stop: %v", round, err)
			}
		}

		rolledBack := queryInt(t, e.database, `SELECT count(*) FROM transition WHERE id = 'auto-rollback:2' AND outcome = 'committed'`)
		base := queryInt(t, e.database, `SELECT coalesce(provisional, 0) FROM base`)
		unaudited := queryInt(t, e.database, `SELECT count(*) FROM transition WHERE outcome = 'committed' AND audited_at IS NULL`)
		switch {
		case rolledBack == 1 && base == 0 && unaudited == 0:
			outcomes["rolled back before the stop"]++
		case rolledBack == 0 && base == 2:
			outcomes["stopped before the rollback"]++
		default:
			t.Fatalf("round %d: rollback %d, provisional %d, unaudited %d", round, rolledBack, base, unaudited)
		}

		again, _ := e.own(e.controllerConfig())
		if g := again.Current(); g == nil || g.Revision != 1 {
			t.Fatalf("round %d: after restart %+v", round, g)
		}
		if err := again.Stop(); err != nil {
			t.Fatal(err)
		}
		if n := queryInt(t, e.database, `SELECT count(*) FROM transition WHERE id = 'auto-rollback:2'`); n != 1 {
			t.Fatalf("round %d: %d rollbacks", round, n)
		}
	}
	t.Logf("outcomes: %v", outcomes)
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

// A controller owned by an application runs the policy lifecycles through its
// own timers and the protected Admin API: a provisional base times out and
// rolls back, another is confirmed, an override expires, every change is
// audited, and a restart ends every administrator session while keeping the
// confirmed authority.
func TestOwnedControllerRunsPolicyLifecycles(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	c, _ := e.own(e.controllerConfig())
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	harness := e.mustLogin(harnessAdmin, harnessPassword)

	_, timed := e.provisionalFrom(harness, e.cpaCandidate(), 1, 1, "owned-timeout")
	if s := e.status(); s.Provisional != timed.Provisional {
		t.Fatalf("status %+v, want provisional %d", s, timed.Provisional)
	}
	waitFor(t, timed.Deadline.Add(5*time.Second), "the automatic rollback", func() bool { return e.status().Provisional == 0 })
	if s := e.status(); s.Confirmed != 1 || s.Revision != 1 {
		t.Fatalf("after the rollback %+v", s)
	}

	_, kept := e.provisionalFrom(harness, e.cpaCandidate(), 1, 60, "owned-confirm")
	if code, _, body := e.resolve(harness, "confirm", kept.Provisional, `"r1"`, "owned-confirm-resolve"); code != http.StatusOK {
		t.Fatalf("confirmation: %d %s", code, body)
	}
	if s := e.status(); s.Confirmed != kept.Provisional || !s.NotAfter.IsZero() {
		t.Fatalf("after the confirmation %+v", s)
	}

	created := e.mustOverride(harness, operationContainment("read-a", 1), policyETag(kept.Provisional), "owned-override-create")
	waitFor(t, created.Expires.Add(5*time.Second), "the override expiry", func() bool { return len(e.status().Overrides) == 0 })

	events := e.eventsWhen(func(events []map[string]any) bool { return countKind(events, "tpo.expired") == 1 })
	for _, kind := range []string{"cpa.activated", "cpa.timed_out", "cpa.confirmed", "tpo.created", "tpo.expired"} {
		if countKind(events, kind) == 0 {
			t.Errorf("no %s event", kind)
		}
	}

	if err := c.Stop(); err != nil {
		t.Fatal(err)
	}
	again, _ := e.own(e.controllerConfig())
	if err := again.Start(); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := e.as(harness, http.MethodGet, "/admin/session", ""); code != http.StatusUnauthorized {
		t.Fatalf("session after restart: %d", code)
	}
	if s := e.status(); s.Confirmed != kept.Provisional || len(s.Overrides) != 0 {
		t.Fatalf("after restart %+v", s)
	}
}
