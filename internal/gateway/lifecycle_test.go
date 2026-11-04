// Application-owned controller helpers, ownership and configuration refusals.

package gateway

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
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
