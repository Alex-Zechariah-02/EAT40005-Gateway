package gateway

import (
	"math/rand/v2"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/compile"
)

// A full table of live counters refuses a new key without scanning every
// counter under the lock.
func TestFullTableRefusalIsBounded(t *testing.T) {
	l := newLimiter()
	rule := quota(10, 60_000)
	now := int64(1_000_000)
	for i := range compile.MaxCounters {
		if l.admit(counterKey{"search", compile.RateKeyAddress, "2001:db8::" + strconv.Itoa(i)}, rule, now) != rateAdmitted {
			t.Fatalf("filling the table: counter %d refused", i)
		}
	}
	const refusals = 100
	start := time.Now()
	for i := range refusals {
		if got := l.admit(counterKey{"search", compile.RateKeyAddress, "2001:db8:1::" + strconv.Itoa(i)}, rule, now); got != rateNoCapacity {
			t.Fatalf("new key with a full table: %v, want no capacity", got)
		}
	}
	if mean := time.Since(start) / refusals; mean > time.Millisecond {
		t.Fatalf("a refusal with a full table took %v on average, want under 1 ms", mean)
	}
}

// The limiter decides exactly as the original single-map limiter with full
// sweeps, and keeps exactly the same counters, for random request sequences.
func TestLimiterMatchesReference(t *testing.T) {
	short, long := quota(3, 1_000), quota(5, 7_000)
	for seed := range uint64(40) {
		random := rand.New(rand.NewPCG(seed, 99))
		capacity := []int{4, 16, 4096}[seed%3]
		l, reference := newLimiter(), newReferenceLimiter()
		l.capacity, reference.capacity = capacity, capacity
		now := int64(random.IntN(5_000))
		for step := range 6_000 {
			// Occasionally steps back; limiter time is Unix milliseconds, never negative.
			now = max(now+int64(random.IntN(400))-20, 0)
			rule := short
			if random.IntN(3) == 0 {
				rule = long // the same key may change window
			}
			key := counterKey{"op", compile.RateKeySubject, strconv.Itoa(random.IntN(40))}
			got, want := l.admit(key, rule, now), reference.admit(key, rule, now)
			if got != want {
				t.Fatalf("seed %d step %d: decision %v, want %v", seed, step, got, want)
			}
			if !slices.Equal(sortedKeys(l.counters), sortedKeys(reference.counters)) {
				t.Fatalf("seed %d step %d: counters differ", seed, step)
			}
		}
	}
}

func sortedKeys(counters map[counterKey]*counter) []string {
	keys := make([]string, 0, len(counters))
	for key := range counters {
		keys = append(keys, key.operation+"/"+key.keyType+"/"+key.key)
	}
	slices.Sort(keys)
	return keys
}

// referenceLimiter is the original limiter: one map, swept in full.
type referenceLimiter struct {
	counters   map[counterKey]*counter
	admissions int
	capacity   int
}

func newReferenceLimiter() *referenceLimiter {
	return &referenceLimiter{counters: map[counterKey]*counter{}, capacity: compile.MaxCounters}
}

func (l *referenceLimiter) admit(key counterKey, rule *compile.RateRule, now int64) rateDecision {
	c := l.counters[key]
	if c == nil {
		if len(l.counters) >= l.capacity {
			l.sweep(now)
		}
		if len(l.counters) >= l.capacity {
			return rateNoCapacity
		}
		c = &counter{window: rule.Window(), bucket: now / rule.Window()}
		l.counters[key] = c
	}
	c.roll(now)
	if c.window != rule.Window() {
		used := (c.usage(now) + c.window - 1) / c.window
		*c = counter{window: rule.Window(), bucket: now / rule.Window(), current: used}
	}
	if c.usage(now)+rule.Window() > rule.Count()*rule.Window() {
		return rateExceeded
	}
	c.current++
	l.admissions++
	if l.admissions%compile.SweepInterval == 0 {
		l.sweep(now)
	}
	return rateAdmitted
}

func (l *referenceLimiter) sweep(now int64) {
	for key, c := range l.counters {
		if now/c.window >= c.bucket+2 {
			delete(l.counters, key)
		}
	}
}
