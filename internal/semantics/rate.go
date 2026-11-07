// Rate primitives: algorithms with their ceiling and guarantee, keys, and conversion to the gateway's sliding window.

package semantics

import (
	"fmt"

	"github.com/Alex-Zechariah-02/apiward/internal/exchange"
)

// Algorithm is a rate-limiting algorithm.
type Algorithm string

const (
	FixedAligned      Algorithm = "fixed_aligned"       // a counter per window aligned to the clock
	FixedFirstRequest Algorithm = "fixed_first_request" // a window starting at a key's first request
	WeightedSliding   Algorithm = "weighted_sliding"    // the gateway's own: current plus weighted previous window
	TokenBucket       Algorithm = "token_bucket"        // Burst tokens, refilled Count per Window
	LeakyBucket       Algorithm = "leaky_bucket"        // Count per Window with Burst excess requests
	SlidingLog        Algorithm = "sliding_log"         // every admitted time kept for one Window
)

// Limits of the gateway's rate rule.
const (
	maxCount  = 1000000
	maxWindow = 86400
)

// Rate is one limit for one key: Count requests per Window seconds; Burst
// for the buckets; Delay when a leaky bucket queues instead of refusing.
type Rate struct {
	Algorithm Algorithm
	Count     int64
	Window    int64
	Burst     int64
	Delay     bool
}

// Ceiling is the most requests the algorithm admits in any interval of
// one window, against an adversarial sender (half-open intervals).
func (r Rate) Ceiling() int64 {
	switch r.Algorithm {
	case FixedAligned:
		return 2 * r.Count
	case WeightedSliding, FixedFirstRequest:
		// A window's first request is at its start, a full window before the next.
		return 2*r.Count - 1
	case TokenBucket:
		return r.Burst + r.Count - 1
	case LeakyBucket:
		return r.Burst + r.Count // a token bucket holding Burst+1
	}
	return r.Count
}

// Guarantee is the most requests a sender may send in every interval of one
// window and never be refused.
func (r Rate) Guarantee() int64 {
	switch r.Algorithm {
	case WeightedSliding:
		return r.Count / 2
	case TokenBucket:
		return min(r.Burst, r.Count)
	case LeakyBucket:
		return min(r.Burst+1, r.Count)
	}
	return r.Count
}

// Convert returns the gateway's count and window for the rate and the
// distance. The window is kept; the count is the source's, lowered when
// needed so the gateway's ceiling 2N-1 is never above the source's.
func (r Rate) Convert() (int64, int64, Distance) {
	switch {
	case r.Count < 1 || r.Count > maxCount:
		return 0, 0, inexpressible(fmt.Sprintf("The gateway's count is 1 to %d.", maxCount))
	case r.Window < 1 || r.Window > maxWindow:
		return 0, 0, inexpressible(fmt.Sprintf("The gateway's window is 1 to %d seconds.", maxWindow))
	case r.Algorithm == LeakyBucket && r.Delay:
		return 0, 0, inexpressible("The source delays excess requests; the gateway never queues a request.")
	case r.Algorithm == WeightedSliding:
		return r.Count, r.Window, exact()
	}
	count := min(r.Count, (r.Ceiling()+1)/2)
	gateway := Rate{Algorithm: WeightedSliding, Count: count, Window: r.Window}
	return count, r.Window, approximated(exchange.Both, fmt.Sprintf(
		"%d per %d s: ceiling %d ≤ %d, guarantee %d %s %d in any %d s.", count, r.Window,
		gateway.Ceiling(), r.Ceiling(), gateway.Guarantee(), compare(gateway.Guarantee(), r.Guarantee()), r.Guarantee(), r.Window))
}

func compare(a, b int64) string {
	switch {
	case a < b:
		return "<"
	case a > b:
		return ">"
	}
	return "="
}

// Key is what a rate is counted by.
type Key string

const (
	KeyCaller        Key = "caller"         // the signed-in caller
	KeyClientAddress Key = "client_address" // the direct client address
	KeyShared        Key = "shared"         // one counter for everyone
	KeyHeader        Key = "header"         // a request header's value
	KeyPath          Key = "path"           // the request path
)

// Convert returns the gateway's rate key, or "" when it has none.
func (k Key) Convert() (string, Distance) {
	switch k {
	case KeyCaller:
		return "subject", exact()
	case KeyClientAddress:
		return "client_address", exact()
	}
	return "", inexpressible("The gateway counts by signed-in caller or client address only.")
}
