// Rate counter transitions, reclamation and concurrency.

package gateway

import (
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Alex-Zechariah-02/apiward/internal/compile"
)

// Counter identity and transitions: the counter belongs to the operation and
// subject, not to the policy generation.
func TestRateCounterTransitions(t *testing.T) {
	key := counterKey{"op", "subject", "alice"}

	t.Run("count change keeps usage", func(t *testing.T) {
		l := newLimiter()
		runSteps(t, "N=10", l, key, quota(10, 60000), repeatStep(6, 1000, true))
		runSteps(t, "N=5", l, key, quota(5, 60000), []step{{2000, false}}) // 6+1 > 5
		runSteps(t, "N=8", l, key, quota(8, 60000), []step{{3000, true}, {3000, true}, {3000, false}})
	})
	t.Run("window change carries usage", func(t *testing.T) {
		l := newLimiter()
		runSteps(t, "W=60s", l, key, quota(10, 60000), repeatStep(8, 1000, true))
		// At 20 s under W=60 s the usage is 8. It becomes the current count of
		// a W=30 s counter, so only 2 more fit.
		runSteps(t, "W=30s", l, key, quota(10, 30000), []step{{20000, true}, {20000, true}, {20000, false}})
		// Next 30 s bucket, d=0: P=10, E = 10: refused.
		runSteps(t, "W=30s next bucket", l, key, quota(10, 30000), []step{{30000, false}})
	})
	t.Run("window change rounds usage up", func(t *testing.T) {
		l := newLimiter()
		runSteps(t, "W=1s", l, key, quota(3, 1000), repeatStep(3, 0, true))
		// At 1.5 s under W=1 s: P=3, d=500, usage 1.5, rounded up to 2. Under
		// W=10 s with N=3 one more fits.
		runSteps(t, "W=10s", l, key, quota(3, 10000), []step{{1500, true}, {1500, false}})
	})
	t.Run("equivalent rule keeps usage", func(t *testing.T) {
		l := newLimiter()
		runSteps(t, "first", l, key, quota(3, 60000), repeatStep(3, 1000, true))
		// A republished generation compiles a new but equal rule object.
		runSteps(t, "republished", l, key, quota(3, 60000), []step{{1000, false}})
	})
	t.Run("rule disabled and enabled keeps usage", func(t *testing.T) {
		l := newLimiter()
		runSteps(t, "enabled", l, key, quota(3, 60000), repeatStep(3, 1000, true))
		// While disabled the limiter is not called. Re-enabled within the
		// window the earlier requests still count.
		runSteps(t, "re-enabled", l, key, quota(3, 60000), []step{{30000, false}})
	})
	t.Run("keys are independent", func(t *testing.T) {
		l := newLimiter()
		rule := quota(2, 60000)
		runSteps(t, "alice", l, key, rule, append(repeatStep(2, 1000, true), step{1000, false}))
		runSteps(t, "bob", l, counterKey{"op", "subject", "bob"}, rule, repeatStep(2, 1000, true))
		runSteps(t, "other operation", l, counterKey{"op2", "subject", "alice"}, rule, repeatStep(2, 1000, true))
		runSteps(t, "renamed operation", l, counterKey{"op-v2", "subject", "alice"}, rule, repeatStep(2, 1000, true))
	})
}

// Expired counters are removed without changing any decision, and a full
// table refuses new keys rather than evicting a live counter.
func TestCounterReclamationAndCapacity(t *testing.T) {
	l := newLimiter()
	l.capacity = 3
	rule := quota(1, 1000)
	for i := range 3 {
		if l.admit(counterKey{"op", "subject", strconv.Itoa(i)}, rule, 0) != rateAdmitted {
			t.Fatal("first request refused")
		}
	}
	if got := l.admit(counterKey{"op", "subject", "new"}, rule, 500); got != rateNoCapacity {
		t.Fatalf("new key with a full table of live counters: %v, want no capacity", got)
	}
	// Live counters keep their counts.
	if l.admit(counterKey{"op", "subject", "0"}, rule, 500) != rateExceeded {
		t.Fatal("a live counter lost its count")
	}
	// At 1.5 s the counters are in their previous bucket: still live.
	if got := l.admit(counterKey{"op", "subject", "new"}, rule, 1500); got != rateNoCapacity {
		t.Fatalf("at 1.5 s: %v, want no capacity", got)
	}
	// From 2 s both buckets of every counter have ended; they are removed.
	if got := l.admit(counterKey{"op", "subject", "new"}, rule, 2000); got != rateAdmitted {
		t.Fatalf("after expiry: %v, want admitted", got)
	}
	if len(l.counters) != 1 {
		t.Fatalf("%d counters after reclamation, want 1", len(l.counters))
	}

	// The periodic sweep removes only expired counters.
	l = newLimiter()
	long := quota(compile.MaxRateCount, 3600000)
	l.admit(counterKey{"long", "subject", "a"}, long, 0)
	for i := range compile.SweepInterval {
		l.admit(counterKey{"short", "subject", strconv.Itoa(i)}, rule, 0)
	}
	l.admit(counterKey{"short", "subject", "late"}, rule, 5000)
	for i := range compile.SweepInterval - 1 {
		l.admit(counterKey{"long", "subject", "a"}, long, 5000+int64(i))
	}
	if _, ok := l.counters[counterKey{"long", "subject", "a"}]; !ok || len(l.counters) > 2 {
		t.Fatalf("after sweeping: %d counters, long-window counter present %v", len(l.counters), ok)
	}
	if c := l.counters[counterKey{"long", "subject", "a"}]; c.current != compile.SweepInterval {
		t.Fatalf("long-window counter holds %d, want %d", c.current, compile.SweepInterval)
	}
}

// Simultaneous admissions never exceed the quota, for one key or many.
func TestConcurrentAdmissionsRespectQuota(t *testing.T) {
	l := newLimiter()
	rule := quota(1000, 3600000)
	var admitted atomic.Int64
	var wg sync.WaitGroup
	for range 64 {
		wg.Go(func() {
			for range 100 {
				if l.admit(counterKey{"op", "subject", "alice"}, rule, 1000) == rateAdmitted {
					admitted.Add(1)
				}
			}
		})
	}
	wg.Wait()
	if admitted.Load() != 1000 {
		t.Fatalf("%d admitted of 6400 simultaneous requests, want exactly 1000", admitted.Load())
	}

	perKey := make([]atomic.Int64, 50)
	for i := range 50 {
		for range 4 {
			wg.Go(func() {
				for range 20 {
					if l.admit(counterKey{"op", "subject", strconv.Itoa(i)}, quota(30, 3600000), 1000) == rateAdmitted {
						perKey[i].Add(1)
					}
				}
			})
		}
	}
	wg.Wait()
	for i := range perKey {
		if perKey[i].Load() != 30 {
			t.Fatalf("subject %d: %d admitted of 80, want 30", i, perKey[i].Load())
		}
	}
}
