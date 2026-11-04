// Override expiry under clock changes.

package gateway

import (
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// The test gateway moves its wall clock alone. A wall clock set back while an
// override is active does not extend it: it expires when the monotonic clock
// reaches its expiry, with no request. A wall clock set forward ends it early
// and no later request uses it. A wall clock behind the latest recorded time
// at startup keeps the gateway unavailable.
func TestOverrideExpiryUnderClockChanges(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	offset := filepath.Join(t.TempDir(), "wall-offset")
	setOffset := func(d time.Duration) {
		if err := os.WriteFile(offset, []byte(strconv.FormatInt(d.Milliseconds(), 10)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	setOffset(0)
	e.start(binaries.fault, "GATEWAY_WALL_OFFSET="+offset)
	harness := e.mustLogin(harnessAdmin, harnessPassword)

	// Set back an hour after creation: still expires on time.
	setBack := e.mustOverride(harness, permitOverride(overrideException, "READER", "write-a", 2), `"r1"`, "clock-set-back-0001")
	setOffset(-time.Hour)
	waitFor(t, setBack.Expires.Add(3*time.Second), "the expiry with the wall clock set back", func() bool { return len(e.status().Overrides) == 0 })
	if e.probe("bob", http.MethodPost, "/a/item") != http.StatusForbidden {
		t.Fatal("grant used after its expiry with the wall clock set back")
	}

	// Set forward an hour: an override with ten minutes left is not used again.
	setOffset(0)
	forward := e.mustOverride(harness, permitOverride(overrideException, "READER", "write-a", 600), `"r1"`, "clock-forward-00001")
	if e.probe("bob", http.MethodPost, "/a/item") != http.StatusOK {
		t.Fatal("grant not enforced")
	}
	setOffset(time.Hour)
	e.resetReceivers()
	if code := e.probe("bob", http.MethodPost, "/a/item"); code == http.StatusOK {
		t.Fatal("grant used after the wall clock passed its expiry")
	}
	waitFor(t, time.Now().Add(5*time.Second), "the early expiry", func() bool { return len(e.status().Overrides) == 0 })
	if code := e.probe("bob", http.MethodPost, "/a/item"); code != http.StatusForbidden {
		t.Fatalf("after the early expiry: %d", code)
	}
	e.assertReceipts(0, 0, 0)

	// Created while the wall clock is an hour ahead, then set back to the
	// time the monotonic clock implies: the override still ends after its own
	// two seconds, not an hour later.
	ahead := e.mustOverride(harness, permitOverride(overrideException, "READER", "write-a", 2), `"r1"`, "clock-ahead-000001")
	setOffset(0)
	if e.probe("bob", http.MethodPost, "/a/item") != http.StatusOK {
		t.Fatal("grant not enforced before its lifetime ended")
	}
	waitFor(t, time.Now().Add(5*time.Second), "the expiry after the clock moved back", func() bool { return len(e.status().Overrides) == 0 })
	if code := e.probe("bob", http.MethodPost, "/a/item"); code != http.StatusForbidden {
		t.Fatalf("grant still used after its lifetime: %d", code)
	}
	setOffset(time.Hour)

	// Startup with the wall clock behind the latest recorded time (written an
	// hour ahead above) refuses to serve; with it restored it serves.
	e.stop()
	setOffset(0)
	e.start(binaries.fault, "GATEWAY_WALL_OFFSET="+offset)
	if health := e.health(); health != "unavailable" || e.unavailableReason() != "clock_regression" {
		t.Fatalf("health %q after the clock moved back", health)
	}
	if code := e.probe("bob", http.MethodGet, "/a/x"); code != http.StatusServiceUnavailable {
		t.Fatalf("data plane with the clock behind: %d", code)
	}
	e.stop()
	setOffset(time.Hour)
	e.start(binaries.fault, "GATEWAY_WALL_OFFSET="+offset)
	if health := e.health(); health != modeNormal {
		t.Fatalf("health %q with the clock restored", health)
	}

	e.stop()
	for _, o := range storedOverrides(t, e.database) {
		if o.Status != overrideExpired {
			t.Fatalf("override %+v", o)
		}
	}
	list := storedOverrides(t, e.database)
	// The set-back override ended within seconds of its real expiry time.
	if first := list[0]; first.ID != setBack.Override || first.ResolvedAt.Sub(first.ExpiresAt) > 2*time.Second || first.ResolvedAt.Before(first.ExpiresAt) {
		t.Fatalf("set-back override %+v ended %v after its expiry", first, first.ResolvedAt.Sub(first.ExpiresAt))
	}
	// The forward override ended about an hour early by real time, at its
	// expiry time by the wall clock it was ended by.
	if second := list[1]; second.ID != forward.Override || second.ResolvedAt.Before(second.ExpiresAt) {
		t.Fatalf("forward override %+v", second)
	}
	// The override created ahead ended within seconds of its expiry time.
	if third := list[2]; third.ID != ahead.Override || third.ResolvedAt.Before(third.ExpiresAt) || third.ResolvedAt.Sub(third.ExpiresAt) > 2*time.Second {
		t.Fatalf("override created ahead %+v ended %v after its expiry", third, third.ResolvedAt.Sub(third.ExpiresAt))
	}
}
