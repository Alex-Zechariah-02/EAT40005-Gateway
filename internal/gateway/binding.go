// The upstream binding carried from enforcement to the proxy, and its errors.

package gateway

import "errors"

// Forwarding failures that decide the gateway's own answer; any other error
// means the upstream could not be reached or failed.
var (
	errNoUpstream      = errors.New("no upstream for the request")
	errUpstreamTimeout = errors.New("upstream timed out")
)

// binding is the trusted upstream decision carried in the request context.
// Only this package can create it; no header or placeholder can.
type binding struct{ dial string }

type bindingKey struct{}
