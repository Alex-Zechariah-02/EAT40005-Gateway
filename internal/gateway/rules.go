// Rate and header rules compiled into a generation.

package gateway

import (
	"net/http"
	"strings"
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
// milliseconds for each verified subject or each direct client address.
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

// isGatewayHeader reports whether only the gateway may set the named header.
func isGatewayHeader(name string) bool {
	name = http.CanonicalHeaderKey(name)
	return name == "X-Request-Id" || strings.HasPrefix(name, "X-Gateway-")
}
