// Rate and header rules compiled into a generation.

package compile

import (
	"net/http"
	"strings"

	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

const (
	RateAlgorithm    = "sliding-window"
	RateKeySubject   = "subject"
	RateKeyAddress   = "client_address"
	MaxRateCount     = 1_000_000
	MaxRateWindowSec = 86_400
	// Subject and public-address cardinality is untrusted. A full table fails
	// closed; expired counters are swept without resetting live quotas.
	MaxCounters = 262144
	// SweepInterval is how many admissions pass between removals of expired
	// counters.
	SweepInterval = 1024
)

// RateRule is a compiled rate rule: at most count requests per window
// milliseconds for each verified subject or each direct client address.
type RateRule struct {
	count  int64
	window int64 // milliseconds
}

func CompileRate(rule policy.RateRule, field string) (*RateRule, error) {
	if rule.Algorithm != RateAlgorithm {
		return nil, policy.Invalid("UNSUPPORTED_FEATURE", "%s.algorithm", field)
	}
	if rule.Key != RateKeySubject && rule.Key != RateKeyAddress {
		return nil, policy.Invalid("UNSUPPORTED_FEATURE", "%s.key", field)
	}
	if rule.Count < 1 || rule.Count > MaxRateCount {
		return nil, policy.Invalid("INVALID_VALUE", "%s.count", field)
	}
	if rule.WindowSeconds < 1 || rule.WindowSeconds > MaxRateWindowSec {
		return nil, policy.Invalid("INVALID_VALUE", "%s.window_seconds", field)
	}
	return &RateRule{count: rule.Count, window: rule.WindowSeconds * 1000}, nil
}

// IsGatewayHeader reports whether only the gateway may set the named header.
func IsGatewayHeader(name string) bool {
	name = http.CanonicalHeaderKey(name)
	return name == "X-Request-Id" || strings.HasPrefix(name, "X-Gateway-")
}
