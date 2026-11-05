// Controller start without authority, ownership and start failures.

package gateway

import (
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Alex-Zechariah-02/apiward/internal/store"
)

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
			seed(e.t, e.database, e.policy(e.a, false), seedOptions{version: store.DatabaseVersion - 1})
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
			if _, err := db.Exec(`UPDATE revision SET document = ?, checksum = ? WHERE id = 1`, earlier, store.DocumentChecksum(earlier)); err != nil {
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
	if first.Current() != g || e.health() != store.ModeNormal {
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
