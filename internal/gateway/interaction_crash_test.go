// Override transitions across crashes and restarts.

package gateway

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/store"
)

// A crash at each step of creating, revoking or expiring an override leaves
// either the state before it (crash before the commit) or the state after it
// (crash after the commit), which a restart serves, completes and audits
// once; a retried request is applied once.
func TestOverrideTransitionsSurviveCrash(t *testing.T) {
	type point struct {
		name      string
		committed bool
	}
	creation := []point{
		{"tpo-create-before-commit", false}, {"tpo-commit-partway", false}, {"tpo-create-after-commit", true},
		{"tpo-create-after-swap", true}, {"tpo-create-after-receipt", true}, {"after-outcome", true},
	}
	for _, p := range creation {
		t.Run("create/"+p.name, func(t *testing.T) {
			e := newEnv(t)
			seed(t, e.database, e.protectedPolicy(), seedOptions{})
			e.start(binaries.fault, "GATEWAY_FAULT="+p.name)
			harness := e.mustLogin(harnessAdmin, harnessPassword)
			request := permitOverride(store.OverrideException, "READER", "write-a", 600)
			if code, _, body := e.createOverride(harness, request, `"r1"`, "crash-create-000001"); code != 0 {
				t.Fatalf("the gateway answered %d %s instead of crashing", code, body)
			}
			if code := e.waitExit(); code != crashExit {
				t.Fatalf("exit code %d", code)
			}
			e.start(binaries.plain)
			harness = e.mustLogin(harnessAdmin, harnessPassword)
			e.resetReceivers()
			wantWrite := http.StatusForbidden
			if p.committed {
				wantWrite = http.StatusOK
			}
			if code := e.probe("bob", http.MethodPost, "/a/item"); code != wantWrite {
				t.Fatalf("after restart: bob write %d, want %d", code, wantWrite)
			}
			// The retry returns the committed receipt, or applies now.
			code, outcome, body := e.createOverride(harness, request, `"r1"`, "crash-create-000001")
			if code != http.StatusCreated || outcome.Override != 1 {
				t.Fatalf("retry: %d %s", code, body)
			}
			if n := len(e.activeOverrides(harness).Overrides); n != 1 {
				t.Fatalf("%d active overrides after the retry", n)
			}
			events := e.eventsWhen(func(events []map[string]any) bool { return countKind(events, "tpo.created") >= 1 })
			if n := countKind(events, "tpo.created"); n != 1 {
				t.Fatalf("%d creation outcome events", n)
			}
			e.stop()
			if n := queryInt(t, e.database, `SELECT COUNT(*) FROM transition WHERE kind = 'tpo.create' AND outcome = 'committed'
				AND published_at IS NOT NULL AND audited_at IS NOT NULL`); n != 1 {
				t.Fatalf("%d complete creation transitions", n)
			}
		})
	}

	ending := []point{{"tpo-revoke-before-commit", false}, {"tpo-revoke-after-commit", true}, {"tpo-revoke-after-swap", true}, {"tpo-revoke-after-receipt", true}}
	for _, p := range ending {
		t.Run("revoke/"+p.name, func(t *testing.T) {
			e := newEnv(t)
			seed(t, e.database, e.protectedPolicy(), seedOptions{})
			e.start(binaries.plain)
			harness := e.mustLogin(harnessAdmin, harnessPassword)
			o := e.mustOverride(harness, permitOverride(store.OverrideException, "READER", "write-a", 600), `"r1"`, "crash-revoke-create")
			e.stop()
			e.start(binaries.fault, "GATEWAY_FAULT="+p.name)
			harness = e.mustLogin(harnessAdmin, harnessPassword)
			if code, _, body := e.revokeOverride(harness, o.Override, "crash-revoke-0000001"); code != 0 {
				t.Fatalf("the gateway answered %d %s instead of crashing", code, body)
			}
			if code := e.waitExit(); code != crashExit {
				t.Fatalf("exit code %d", code)
			}
			e.start(binaries.plain)
			harness = e.mustLogin(harnessAdmin, harnessPassword)
			wantWrite := http.StatusOK
			if p.committed {
				wantWrite = http.StatusForbidden
			}
			if code := e.probe("bob", http.MethodPost, "/a/item"); code != wantWrite {
				t.Fatalf("after restart: bob write %d, want %d", code, wantWrite)
			}
			code, outcome, body := e.revokeOverride(harness, o.Override, "crash-revoke-0000001")
			if code != http.StatusOK || outcome.Disposition != store.OverrideRevoked {
				t.Fatalf("retry: %d %s", code, body)
			}
			if e.probe("bob", http.MethodPost, "/a/item") != http.StatusForbidden {
				t.Fatal("revoked grant enforced after the retry")
			}
			events := e.eventsWhen(func(events []map[string]any) bool { return countKind(events, "tpo.revoked") >= 1 })
			if n := countKind(events, "tpo.revoked"); n != 1 {
				t.Fatalf("%d revocation outcome events", n)
			}
		})
	}

	for _, p := range []point{{"tpo-expire-before-commit", false}, {"tpo-expire-after-commit", true}, {"tpo-expire-after-swap", true}} {
		t.Run("expire/"+p.name, func(t *testing.T) {
			e := newEnv(t)
			seed(t, e.database, e.protectedPolicy(), seedOptions{})
			e.start(binaries.fault, "GATEWAY_FAULT="+p.name)
			harness := e.mustLogin(harnessAdmin, harnessPassword)
			o := e.mustOverride(harness, permitOverride(store.OverrideException, "READER", "write-a", 1), `"r1"`, "crash-expire-create")
			// The expiry timer ends the override and the process dies there.
			if code := e.waitExit(); code != crashExit {
				t.Fatalf("exit code %d", code)
			}
			e.start(binaries.plain)
			harness = e.mustLogin(harnessAdmin, harnessPassword)
			e.resetReceivers()
			if code := e.probe("bob", http.MethodPost, "/a/item"); code != http.StatusForbidden {
				t.Fatalf("first write after restarting past the expiry: %d", code)
			}
			e.assertReceipts(0, 0, 0)
			got, found, _ := func() (store.Override, bool, error) {
				code, body, _ := e.as(harness, http.MethodGet, fmt.Sprintf("/admin/overrides/%d", o.Override), "")
				if code != http.StatusOK {
					return store.Override{}, false, nil
				}
				return fromJSON[store.Override](t, body), true, nil
			}()
			if !found || got.Status != store.OverrideExpired || got.Resolution != fmt.Sprintf("auto-expire:%d", o.Override) || got.ResolvedAt.Before(got.ExpiresAt) {
				t.Fatalf("override after restart %+v", got)
			}
			events := e.eventsWhen(func(events []map[string]any) bool { return countKind(events, "tpo.expired") >= 1 })
			if n := countKind(events, "tpo.expired"); n != 1 {
				t.Fatalf("%d expiry outcome events", n)
			}
		})
	}
}

// A restart before an override's expiry serves it again with the same
// expiry time; a gateway stopped across the expiry ends it before serving.
// A provisional base with overrides restarts as such, and a crash during a
// rollback while an override expires is reconciled to the confirmed base
// with only the overrides still active.
func TestRestartWithOverrides(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.plain)
	harness := e.mustLogin(harnessAdmin, harnessPassword)

	kept := e.mustOverride(harness, operationContainment("read-b", 600), `"r1"`, "restart-kept-000001")
	e.stop()
	e.start(binaries.plain)
	harness = e.mustLogin(harnessAdmin, harnessPassword)
	view := e.activeOverrides(harness)
	if len(view.Overrides) != 1 || !view.Overrides[0].ExpiresAt.Equal(kept.Expires) || view.Generation != kept.Generation {
		t.Fatalf("after restart %+v, created %+v", view, kept)
	}
	if e.probe("bob", http.MethodGet, "/b/x") != http.StatusForbidden {
		t.Fatal("override not enforced after restart")
	}

	short := e.mustOverride(harness, permitOverride(store.OverrideException, "READER", "write-a", 2), `"r1"`, "restart-short-00001")
	e.stop()
	time.Sleep(time.Until(short.Expires.Add(500 * time.Millisecond)))
	// Without the deadline timer, only startup itself can have ended the
	// override before the first request.
	e.start(binaries.fault, "GATEWAY_FAULT=no-deadline-timer")
	e.resetReceivers()
	if code := e.probe("bob", http.MethodPost, "/a/item"); code != http.StatusForbidden {
		t.Fatalf("first write after restarting past the expiry: %d", code)
	}
	e.assertReceipts(0, 0, 0)
	harness = e.mustLogin(harnessAdmin, harnessPassword)
	if s := e.status(); !slices.Equal(s.Overrides, []int64{kept.Override}) {
		t.Fatalf("status %+v", s)
	}

	// A provisional base with overrides restarts as it was.
	expiring := e.mustOverride(harness, permitOverride(store.OverrideContainment, "WRITER", "read-a", 3), `"r1"`, "restart-expiring-01")
	_, activated := e.provisionalFrom(harness, e.upstreamCandidate(), 1, 60, "restart-provisional")
	e.stop()
	e.start(binaries.plain)
	harness = e.mustLogin(harnessAdmin, harnessPassword)
	if s := e.status(); s.Provisional != activated.Provisional || s.Generation != activated.Generation {
		t.Fatalf("provisional status after restart %+v, activation %+v", s, activated)
	}
	// Crash after the rollback is committed, once the containment expired.
	e.stop()
	e.start(binaries.fault, "GATEWAY_FAULT=rollback-after-commit")
	harness = e.mustLogin(harnessAdmin, harnessPassword)
	waitFor(t, expiring.Expires.Add(5*time.Second), "the expiry", func() bool { return len(e.status().Overrides) == 1 })
	if code, _, body := e.resolve(harness, "rollback", activated.Provisional, `"r1"`, "restart-rollback-crash"); code != 0 {
		t.Fatalf("rollback answered %d %s instead of crashing", code, body)
	}
	if code := e.waitExit(); code != crashExit {
		t.Fatalf("exit code %d", code)
	}
	e.start(binaries.plain)
	harness = e.mustLogin(harnessAdmin, harnessPassword)
	s := e.status()
	if s.Confirmed != 1 || s.Provisional != 0 || !strings.HasPrefix(s.Generation, fmt.Sprintf("r1+o%d-", kept.Override)) {
		t.Fatalf("status after the crashed rollback %+v", s)
	}
	e.resetReceivers()
	if e.probe("alice", http.MethodGet, "/a/x") != http.StatusOK || e.probe("bob", http.MethodGet, "/b/x") != http.StatusForbidden {
		t.Fatal("enforcement after the crashed rollback")
	}
	e.assertReceipts(1, 0, 0)
	e.stop()
	if n := queryInt(t, e.database, `SELECT COUNT(*) FROM transition WHERE kind = 'cpa.rollback' AND outcome = 'committed' AND published_at IS NOT NULL AND audited_at IS NOT NULL`); n != 1 {
		t.Fatalf("%d reconciled rollbacks", n)
	}
}
