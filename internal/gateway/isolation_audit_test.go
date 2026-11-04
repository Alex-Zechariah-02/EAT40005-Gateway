// Audit gate and secret safety.

package gateway

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	data, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, data, 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestAuditGateAndSecretSafety(t *testing.T) {
	t.Run("records outcomes without secrets", func(t *testing.T) {
		e := newEnv(t)
		seed(t, e.database, e.policy(e.a, false), seedOptions{})
		// An informational response before the final one must not be
		// recorded as the outcome.
		e.a.handle = func(w http.ResponseWriter, r *http.Request) bool {
			w.WriteHeader(http.StatusEarlyHints)
			return false
		}
		e.start(binaries.fault)
		g := e.status().Generation
		bearer := "Bearer SECRET-TOKEN-1234567890"

		e.get("/a/x?token=SECRET-QUERY-VALUE", "Authorization", bearer)
		e.get("/d?token=SECRET-QUERY-VALUE", "Authorization", bearer)
		e.raw("GET /a/x%0A{\"kind\":\"forged\"} HTTP/1.1\r\nHost: gw\r\nConnection: close\r\n\r\n")
		e.assertReceipts(1, 0, 0)

		// auditEvents fails the test if any line is not exactly one JSON record.
		events := e.eventsWhen(func(events []map[string]any) bool { return countKind(events, "request.rejected") >= 2 })
		raw, _ := os.ReadFile(e.auditLog)
		for _, secret := range []string{"SECRET-TOKEN", "SECRET-QUERY-VALUE", "Bearer"} {
			if strings.Contains(string(raw), secret) {
				t.Fatalf("audit log contains %q", secret)
			}
		}
		seen := map[string]bool{}
		for _, ev := range events {
			if ev["kind"] == "forged" {
				t.Fatal("a request created a forged audit record")
			}
			if ev["kind"] == "request.forwarded" && ev["status"] != float64(http.StatusOK) {
				t.Fatalf("forwarded event %v, want final status 200", ev)
			}
			if strings.HasPrefix(ev["kind"].(string), "request.") {
				key := ev["kind"].(string) + "/" + ev["stage"].(string)
				seen[key] = true
				if ev["stage"] != "CAPTURE" && ev["generation"] != g {
					t.Fatalf("event %v lacks generation %s", ev, g)
				}
			}
		}
		for _, key := range []string{"request.admitted/AUDIT", "request.forwarded/FORWARD", "request.rejected/ACCESS", "request.rejected/CANONICAL"} {
			if !seen[key] {
				t.Fatalf("missing event %s in %v", key, events)
			}
		}
	})

	// An audit log the gateway cannot open stops its startup: nothing listens,
	// so nothing is admitted without its durable record, and the database is
	// released.
	unopenable := []struct {
		name    string
		prepare func(e *env)
	}{
		{"audit file cannot be opened", func(e *env) { os.Mkdir(e.auditLog, 0o700) }},
		{"audit file read-only", func(e *env) {
			os.WriteFile(e.auditLog, nil, 0o600)
			os.Chmod(e.auditLog, 0o400)
			e.t.Cleanup(func() { os.Chmod(e.auditLog, 0o600) })
		}},
		{"audit path is a symbolic link", func(e *env) {
			target := filepath.Join(e.dir, "elsewhere.log")
			os.WriteFile(target, nil, 0o600)
			if err := os.Symlink(target, e.auditLog); err != nil {
				e.t.Fatalf("creating the symbolic link: %v", err)
			}
		}},
	}
	for _, f := range unopenable {
		t.Run(f.name, func(t *testing.T) {
			e := newEnv(t)
			seed(t, e.database, e.policy(e.a, false), seedOptions{})
			f.prepare(e)
			e.run(binaries.plain)
			if code := e.waitExit(); code != exitFailed || !strings.Contains(e.output.String(), "audit log") {
				t.Fatalf("exit %d, want %d naming the audit log\n%s", code, exitFailed, e.output)
			}
			if dialable(e.dataAddr) || dialable(e.admin) {
				t.Fatal("a listener is open after the refused start")
			}
			e.assertReceipts(0, 0, 0)
			st, err := openStore(e.database)
			if err != nil {
				t.Fatalf("the database is held: %v", err)
			}
			st.close()
		})
	}

	// An audit log that fails while the gateway runs stops every protected
	// admission and every policy change.
	failures := []struct {
		name  string
		fault string
	}{
		{"audit sync fails", "GATEWAY_FAULT=audit-sync"},
		{"no space left", "GATEWAY_FAULT=audit-full"},
	}
	for _, f := range failures {
		t.Run(f.name, func(t *testing.T) {
			e := newEnv(t)
			seed(t, e.database, e.policy(e.a, false), seedOptions{})
			e.start(binaries.fault, f.fault)
			before := e.status()
			for range 3 {
				if code, _ := e.get("/a/x"); code != http.StatusServiceUnavailable {
					t.Fatalf("admission with failed audit log: %d", code)
				}
			}
			// The unauthenticated health check reports the stored mode only:
			// it answers 200 and normal while every protected admission
			// fails. The authenticated status below reports the failure.
			if health := e.health(); health != modeNormal {
				t.Fatalf("health %q with a failed audit log", health)
			}
			if code, _ := e.get("/zzz"); code != http.StatusNotFound {
				t.Fatalf("rejection with failed audit log: %d", code)
			}
			e.assertReceipts(0, 0, 0)
			// A policy change needs its durable audit intent first, so the
			// membership API refuses to publish anything.
			code, body, _ := e.harnessCall(http.MethodPut, "/admin/blocked-accounts/someone", `{"reason":"test containment"}`,
				"If-Match", `"r1"`, "Idempotency-Key", "audit-failure-probe")
			if code != http.StatusServiceUnavailable || !strings.Contains(body, "gateway_unavailable") {
				t.Fatalf("publication with failed audit log: %d %s", code, body)
			}
			if s := e.status(); !s.AuditFailed || s.Generation != before.Generation {
				t.Fatalf("status %+v", s)
			}
		})
	}
}

func port(addr string) string {
	_, p, _ := net.SplitHostPort(addr)
	return p
}
