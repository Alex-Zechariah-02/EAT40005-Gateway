// The upstream binding carried from enforcement to the proxy, and its errors.

package forward

import (
	"context"
	"errors"

	"github.com/Alex-Zechariah-02/apiward/internal/compile"
)

// Forwarding failures that decide the gateway's own answer; any other error
// means the upstream could not be reached or failed.
var (
	ErrNoUpstream      = errors.New("no upstream for the request")
	ErrUpstreamTimeout = errors.New("upstream timed out")
)

// binding is the trusted upstream decision carried in the request context:
// the address to dial and the path to forward. Only this package can create
// it; no header or placeholder can.
type binding struct{ dial, path string }

type bindingKey struct{}

// Bind returns ctx carrying the destination the compiled rule chose: the one
// upstream the request may reach and the path it is forwarded with. It is
// the only way to attach a binding.
func Bind(ctx context.Context, d compile.Destination) context.Context {
	return context.WithValue(ctx, bindingKey{}, binding{dial: d.Dial(), path: d.Path()})
}

// Bound returns the address and path ctx is bound to, if a binding is attached.
func Bound(ctx context.Context) (dial, path string, bound bool) {
	b, bound := ctx.Value(bindingKey{}).(binding)
	return b.dial, b.path, bound
}
