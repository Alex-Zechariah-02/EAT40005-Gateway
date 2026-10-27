package gateway

import (
	"sync"
	"time"
)

const (
	rateAlgorithm    = "sliding-window"
	rateKeySubject   = "subject"
	rateKeyAddress   = "client_address"
	maxRateCount     = 1_000_000
	maxRateWindowSec = 86_400
	// Subject and public-address cardinality is untrusted. A full table fails
	// closed; expired counters are swept without resetting live quotas.
	maxCounters = 262144
	// sweepInterval is how many admissions pass between removals of expired
	// counters.
	sweepInterval = 1024
)

// rateRule is a compiled rate rule: at most count requests per window
// milliseconds for each verified subject.
type rateRule struct {
	count  int64
	window int64 // milliseconds
}

func compileRate(rule RateRule, field string) (*rateRule, error) {
	if rule.Algorithm != rateAlgorithm {
		return nil, invalid("UNSUPPORTED_FEATURE", "%s.algorithm", field)
	}
	if rule.Key != rateKeySubject && rule.Key != rateKeyAddress {
		return nil, invalid("UNSUPPORTED_FEATURE", "%s.key", field)
	}
	if rule.Count < 1 || rule.Count > maxRateCount {
		return nil, invalid("INVALID_VALUE", "%s.count", field)
	}
	if rule.WindowSeconds < 1 || rule.WindowSeconds > maxRateWindowSec {
		return nil, invalid("INVALID_VALUE", "%s.window_seconds", field)
	}
	return &rateRule{count: rule.Count, window: rule.WindowSeconds * 1000}, nil
}

// counterKey identifies one quota: an operation and the trusted key it is
// counted by. It does not include the generation, so publishing a policy
// never grants a fresh quota.
type counterKey struct {
	operation string
	keyType   string
	key       string
}

// counter is the state of one approximate sliding window: the requests
// admitted in the current bucket and in the bucket before it. Buckets are
// window milliseconds long and anchored at the Unix epoch.
type counter struct {
	window   int64
	bucket   int64
	current  int64
	previous int64
}

// roll moves the counter to the bucket containing now. A time before the
// current bucket leaves it unchanged.
func (c *counter) roll(now int64) {
	bucket := now / c.window
	switch {
	case bucket == c.bucket+1:
		c.previous, c.current = c.current, 0
	case bucket > c.bucket+1:
		c.previous, c.current = 0, 0
	}
	if bucket > c.bucket {
		c.bucket = bucket
	}
}

// usage returns the weighted request count multiplied by the window, so the
// admission rule is exact integer arithmetic. A time before the bucket start
// weighs the previous bucket fully.
func (c *counter) usage(now int64) int64 {
	elapsed := max(now-c.bucket*c.window, 0)
	return c.current*c.window + c.previous*(c.window-elapsed)
}

type rateDecision int

const (
	rateAdmitted rateDecision = iota
	rateExceeded
	rateNoCapacity
)

// limiter holds the counters of every rate rule on this gateway node. One
// mutex makes each check and increment a single atomic decision.
type limiter struct {
	mu         sync.Mutex
	counters   map[counterKey]*counter
	admissions int
	capacity   int
}

func newLimiter() *limiter {
	return &limiter{counters: map[counterKey]*counter{}, capacity: maxCounters}
}

// admit applies the rule at now (Unix milliseconds): with C requests admitted
// in the current bucket, P in the previous one and offset d into the current
// bucket, the request is admitted when C + P*(W-d)/W + 1 <= N, and only an
// admitted request is counted.
func (l *limiter) admit(key counterKey, rule *rateRule, now int64) rateDecision {
	l.mu.Lock()
	defer l.mu.Unlock()

	c := l.counters[key]
	if c == nil {
		if len(l.counters) >= l.capacity {
			l.sweep(now)
		}
		if len(l.counters) >= l.capacity {
			return rateNoCapacity
		}
		c = &counter{window: rule.window, bucket: now / rule.window}
		l.counters[key] = c
	}

	c.roll(now)
	if c.window != rule.window {
		// The window changed: carry the usage so far, rounded up, into a
		// counter for the new window, so a change never forgives requests.
		used := (c.usage(now) + c.window - 1) / c.window
		*c = counter{window: rule.window, bucket: now / rule.window, current: used}
	}

	if c.usage(now)+rule.window > rule.count*rule.window {
		return rateExceeded
	}
	c.current++

	l.admissions++
	if l.admissions%sweepInterval == 0 {
		l.sweep(now)
	}
	return rateAdmitted
}

// sweep removes counters whose current and previous buckets have both ended.
// Their estimate is zero, so removing them never changes a decision.
func (l *limiter) sweep(now int64) {
	for key, c := range l.counters {
		if now/c.window >= c.bucket+2 {
			delete(l.counters, key)
		}
	}
}

// rateClock reads limiter time: a wall-clock anchor taken once plus the
// monotonic time elapsed since, so wall-clock steps do not move it.
type rateClock struct{ anchor time.Time }

func (c rateClock) now() int64 {
	if ms, ok := rateClockFault(); ok {
		return ms
	}
	return c.anchor.UnixMilli() + time.Since(c.anchor).Milliseconds()
}
