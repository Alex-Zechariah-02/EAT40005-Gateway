// Override expiry and expired overrides.

package gateway

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// An override expires at its expiry time without any request or
// administrator action; the base then applies again. A revocation decided at
// or after the expiry time finds the override expired, and a revocation
// racing the expiry produces one final disposition.
func TestOverrideExpiry(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.plain)
	harness := e.mustLogin(harnessAdmin, harnessPassword)
	baseGeneration := e.status().Generation

	// Expiry with no traffic: only status reads while the time passes.
	kept := e.mustOverride(harness, operationContainment("read-b", 600), `"r1"`, "expiry-kept-000001")
	expiring := e.mustOverride(harness, permitOverride(overrideException, "READER", "write-a", 2), `"r1"`, "expiry-quiet-00001")
	waitFor(t, expiring.Expires.Add(5*time.Second), "the expiry", func() bool { return len(e.status().Overrides) == 1 })
	s := e.status()
	if !strings.HasPrefix(s.Generation, fmt.Sprintf("r1+o%d-", kept.Override)) {
		t.Fatalf("status after the expiry %+v", s)
	}
	e.resetReceivers()
	if e.probe("bob", http.MethodPost, "/a/item") != http.StatusForbidden || e.probe("bob", http.MethodGet, "/b/x") != http.StatusForbidden {
		t.Fatal("after the expiry, the base and the remaining override are not what is enforced")
	}
	e.assertReceipts(0, 0, 0)
	code, outcome, body := e.revokeOverride(harness, expiring.Override, "expiry-late-revoke")
	if code != http.StatusConflict || outcome.Reason != "ALREADY_RESOLVED" || outcome.Disposition != overrideExpired {
		t.Fatalf("revocation after the expiry: %d %s", code, body)
	}
	if code, _, body := e.revokeOverride(harness, kept.Override, "expiry-kept-revoke"); code != http.StatusOK {
		t.Fatalf("revocation: %d %s", code, body)
	}
	if s := e.status(); s.Generation != baseGeneration {
		t.Fatalf("generation %s after all overrides ended, base generation %s", s.Generation, baseGeneration)
	}

	// Revocations racing the expiry, several rounds.
	for round := range 4 {
		o := e.mustOverride(harness, permitOverride(overrideException, "READER", "write-a", 1), `"r1"`, fmt.Sprintf("expiry-race-%02d-xxx", round))
		time.Sleep(time.Until(o.Expires.Add(time.Duration(round*5-10) * time.Millisecond)))
		code, outcome, body := e.revokeOverride(harness, o.Override, fmt.Sprintf("expiry-race-%02d-rev", round))
		switch {
		case code == http.StatusOK && outcome.Disposition == overrideRevoked:
		case code == http.StatusConflict && outcome.Disposition == overrideExpired:
		default:
			t.Fatalf("round %d: %d %s", round, code, body)
		}
	}
	waitFor(t, time.Now().Add(5*time.Second), "every override to end", func() bool { return len(e.status().Overrides) == 0 })

	events := e.eventsWhen(func(events []map[string]any) bool { return countKind(events, "tpo.expired") >= 1 })
	ev := findTransition(events, "tpo.expired", fmt.Sprintf("auto-expire:%d", expiring.Override))
	if ev == nil || ev["actor"] != "controller" || ev["override"] != float64(expiring.Override) || ev["revision"] != float64(1) {
		t.Fatalf("expiry event %v", ev)
	}
	e.stop()
	for _, o := range storedOverrides(t, e.database) {
		resolution := fmt.Sprintf("auto-expire:%d", o.ID)
		switch {
		case o.Status == overrideExpired && o.ResolvedBy == "controller" && o.Resolution == resolution:
			// Expired no earlier than its expiry time, and promptly after it.
			if o.ResolvedAt.Before(o.ExpiresAt) || o.ResolvedAt.Sub(o.ExpiresAt) > time.Second {
				t.Fatalf("override %d expired %v after its expiry time", o.ID, o.ResolvedAt.Sub(o.ExpiresAt))
			}
		case o.Status == overrideRevoked && o.ResolvedBy == harnessAdmin:
			if !o.ResolvedAt.Before(o.ExpiresAt) {
				t.Fatalf("override %d revoked at or after its expiry time", o.ID)
			}
		default:
			t.Fatalf("override %+v has no single valid disposition", o)
		}
		n := queryInt(t, e.database, `SELECT COUNT(*) FROM transition WHERE outcome = 'committed' AND (kind = 'tpo.revoke' OR kind = 'tpo.expire')
			AND id = ?`, o.Resolution)
		if n != 1 {
			t.Fatalf("override %d has %d ending transitions", o.ID, n)
		}
	}
	if n := queryInt(t, e.database, `SELECT COUNT(*) FROM transition WHERE outcome = 'committed' AND kind IN ('tpo.revoke', 'tpo.expire')`); n != 6 {
		t.Fatalf("%d ending transitions for 6 overrides", n)
	}
}

// With the expiry timer disabled (fault build), no request sent at or after
// an override's expiry time is admitted under it: the first request that
// sees the expired generation is refused and makes the controller end the
// override, once, however many see it at once. A revocation decided just
// before the expiry time revokes; one decided at or after it finds the
// override expired.
func TestExpiredOverrideAdmitsNothing(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.fault, "GATEWAY_FAULT=no-deadline-timer")
	harness := e.mustLogin(harnessAdmin, harnessPassword)

	o := e.mustOverride(harness, permitOverride(overrideException, "READER", "write-a", 2), `"r1"`, "late-timer-traffic")
	bobToken := "Bearer " + issuer.tokenFor(t, "bob")
	e.resetReceivers()
	var admittedBefore, refusedAfter, refusedStraddling, baseAfter int
	for time.Now().Before(o.Expires.Add(1500 * time.Millisecond)) {
		sent := time.Now()
		code, _ := e.request(http.MethodPost, "/a/item", "application/json", `{}`, "Authorization", bobToken)
		atOrAfter := !sent.Before(o.Expires)
		switch {
		case code == http.StatusOK && atOrAfter:
			t.Fatalf("write sent %v after the expiry was admitted", sent.Sub(o.Expires))
		case code == http.StatusOK:
			admittedBefore++
		case code == http.StatusServiceUnavailable && atOrAfter:
			refusedAfter++
		case code == http.StatusServiceUnavailable && o.Expires.Sub(sent) < 200*time.Millisecond:
			// Sent just before the expiry but captured at or after it.
			refusedStraddling++
		case code == http.StatusForbidden && atOrAfter:
			baseAfter++
		default:
			t.Fatalf("write sent %v from the expiry: %d", sent.Sub(o.Expires), code)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The request that first sees the expired generation is refused and wakes
	// the controller; it may have been sent just before the expiry time, and
	// then every later request already sees the base.
	if admittedBefore == 0 || refusedAfter+refusedStraddling == 0 || baseAfter == 0 {
		t.Fatalf("admitted before %d, refused after %d (+%d sent just before), base after %d", admittedBefore, refusedAfter, refusedStraddling, baseAfter)
	}
	if n := e.a.count(); n != admittedBefore {
		t.Fatalf("A received %d writes, %d admitted", n, admittedBefore)
	}

	// Many requests see the expiry at once; one expiry happens.
	crowded := e.mustOverride(harness, permitOverride(overrideException, "READER", "write-a", 1), `"r1"`, "late-timer-crowded")
	time.Sleep(time.Until(crowded.Expires.Add(50 * time.Millisecond)))
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 20 {
		wg.Go(func() {
			<-start
			e.get("/b/x", "Authorization", bobToken)
		})
	}
	close(start)
	wg.Wait()
	waitFor(t, time.Now().Add(5*time.Second), "the expiry after concurrent requests", func() bool { return len(e.status().Overrides) == 0 })

	// Revocations at the decision instant around the expiry time.
	for i, offset := range []time.Duration{-40 * time.Millisecond, -10 * time.Millisecond, 0, 10 * time.Millisecond, 40 * time.Millisecond} {
		o := e.mustOverride(harness, permitOverride(overrideException, "READER", "write-a", 1), `"r1"`, fmt.Sprintf("boundary-create-%02d", i))
		time.Sleep(time.Until(o.Expires.Add(offset)))
		sent := time.Now()
		code, outcome, body := e.revokeOverride(harness, o.Override, fmt.Sprintf("boundary-revoke-%02d", i))
		switch {
		case code == http.StatusOK && outcome.Disposition == overrideRevoked:
			if !sent.Before(o.Expires) {
				t.Fatalf("revocation sent %v after the expiry was committed", sent.Sub(o.Expires))
			}
		case code == http.StatusConflict && outcome.Disposition == overrideExpired:
		default:
			t.Fatalf("offset %v: %d %s", offset, code, body)
		}
		t.Logf("revocation sent %v from the expiry: %d %s", sent.Sub(o.Expires), code, outcome.Disposition)
	}

	e.stop()
	for _, o := range storedOverrides(t, e.database) {
		switch o.Status {
		case overrideExpired:
			if o.ResolvedAt.Before(o.ExpiresAt) || o.Resolution != fmt.Sprintf("auto-expire:%d", o.ID) {
				t.Fatalf("expired override %+v", o)
			}
		case overrideRevoked:
			if !o.ResolvedAt.Before(o.ExpiresAt) {
				t.Fatalf("revoked override %+v decided at or after its expiry", o)
			}
		default:
			t.Fatalf("override %+v", o)
		}
	}
}
