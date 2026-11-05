// The upstream binding carried from enforcement to the proxy, and its errors.

package forward

import (
	"context"
	"errors"
)

// Forwarding failures that decide the gateway's own answer; any other error
// means the upstream could not be reached or failed.
var (
	ErrNoUpstream      = errors.New("no upstream for the request")
	ErrUpstreamTimeout = errors.New("upstream timed out")
)

// binding is the trusted upstream decision carried in the request context.
// Only this package can create it; no header or placeholder can.
type binding struct{ dial string }

type bindingKey struct{}

// Bind returns ctx carrying the binding to dial: the one upstream the
// request may reach. It is the only way to attach a binding.
func Bind(ctx context.Context, dial string) context.Context {
	return context.WithValue(ctx, bindingKey{}, binding{dial: dial})
}

// BoundDial returns the address ctx is bound to, if a binding is attached.
func BoundDial(ctx context.Context) (string, bool) {
	b, bound := ctx.Value(bindingKey{}).(binding)
	return b.dial, bound
}
