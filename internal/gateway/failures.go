// Forwarding failure classification.

package gateway

import (
	"errors"
	"net/http"
)

// forwardFailure maps a forwarding failure to the gateway's answer: 504 when
// the upstream timed out, 503 when no upstream could be used, and 502
// otherwise.
func forwardFailure(err error) (int, string) {
	switch {
	case errors.Is(err, errUpstreamTimeout):
		return http.StatusGatewayTimeout, "upstream_timeout"
	case errors.Is(err, errNoUpstream):
		return http.StatusServiceUnavailable, "gateway_unavailable"
	}
	return http.StatusBadGateway, "upstream_unavailable"
}

// failureReason states what is known about the delivery of a request whose
// forwarding failed before any response: "not_sent" when it never reached
// the upstream, "outcome_unknown" when the upstream may have acted on it, and
// "client_cancelled" when the client went away after it was handed to an
// upstream connection, which leaves the outcome unknown too. A forwarder that
// cannot tell gives no reason.
func failureReason(err error) string {
	switch {
	case errors.Is(err, errNotSent):
		return "not_sent"
	case errors.Is(err, errClientGone):
		return "client_cancelled"
	case errors.Is(err, errMaybeSent):
		return "outcome_unknown"
	}
	return ""
}
