// An application-owned controller runs policy lifecycles.

package gateway

import (
	"net/http"
	"testing"
	"time"
)

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

	recorded := []string{"cpa.activated", "cpa.timed_out", "cpa.confirmed", "tpo.created", "tpo.expired"}
	events := e.eventsWhen(func(events []map[string]any) bool { return hasKinds(events, recorded...) })
	for _, kind := range recorded {
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
