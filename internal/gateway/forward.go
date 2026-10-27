package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/http/httputil"
	"net/url"
	"strconv"
	"sync/atomic"
	"time"
)

// upstreamTimeouts bounds the phases of one exchange with an upstream.
type upstreamTimeouts struct {
	// dial bounds establishing the connection.
	dial time.Duration
	// responseHeader bounds the wait for the response header once the
	// request has been written.
	responseHeader time.Duration
	// readIdle bounds each wait for more of the response body: a body that
	// sends nothing for this long ends the exchange.
	readIdle time.Duration
}

// Delivery of a request whose exchange failed before any response, recorded
// as the reason of its outcome. Only a request that never had a connection is
// known not to have reached the upstream; once it had one, the upstream may
// have acted on it, whatever the gateway answers.
var (
	errNotSent    = errors.New("the request was not sent")
	errMaybeSent  = errors.New("the request may have reached the upstream")
	errClientGone = errors.New("the client cancelled the request")
)

var (
	errResponseStalled  = errors.New("upstream response stalled")
	errProtocolSwitched = errors.New("upstream switched protocols")
)

// upstreamProxy forwards admitted requests to the upstream bound to them,
// using the standard library's reverse proxy over one dedicated transport.
type upstreamProxy struct {
	transport *http.Transport
	readIdle  time.Duration
	errorLog  *log.Logger
}

// newUpstreamProxy returns the proxy whose exchanges are bounded by timeouts.
// Every timeout must be positive: a zero would leave its phase unbounded.
// The reverse proxy's own warnings, such as a failed response body read, go to
// logger.
func newUpstreamProxy(timeouts upstreamTimeouts, logger *slog.Logger) (*upstreamProxy, error) {
	if timeouts.dial <= 0 || timeouts.responseHeader <= 0 || timeouts.readIdle <= 0 {
		return nil, errors.New("every upstream timeout must be positive")
	}
	if logger == nil {
		return nil, errors.New("a logger is required")
	}
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	return &upstreamProxy{
		transport: &http.Transport{
			// No Proxy function: the bound address is dialled directly, never
			// through a proxy named by the environment.
			DialContext:           (&net.Dialer{Timeout: timeouts.dial}).DialContext,
			ResponseHeaderTimeout: timeouts.responseHeader,
			// Every request is sent once, on a new connection. A transport that
			// keeps idle connections resends a request it deems replayable (GET,
			// HEAD, OPTIONS, or any request with an Idempotency-Key and no body)
			// when a reused connection fails, though the upstream may already
			// have acted on it.
			DisableKeepAlives: true,
			// The upstream receives the client's Accept-Encoding, not one the
			// transport adds, and the client receives the upstream's bytes.
			DisableCompression: true,
			Protocols:          protocols,
		},
		readIdle: timeouts.readIdle,
		errorLog: slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}, nil
}

// forward sends an admitted request to the upstream bound to it and copies
// the response to w; it is the enforcement handler's forwarder. The
// destination is the binding alone, and a request without a usable binding is
// sent nowhere. A failure before any response is returned for the handler to
// answer; a response that fails after it started is aborted by the reverse
// proxy with http.ErrAbortHandler, which it panics with only under a net/http
// server, so the handler must be served by one.
func (p *upstreamProxy) forward(w http.ResponseWriter, r *http.Request) error {
	b, bound := r.Context().Value(bindingKey{}).(binding)
	if !bound || !isLoopbackDial(b.dial) {
		return fmt.Errorf("%w: %w", errNoUpstream, errNotSent)
	}

	// The exchange ends when the client goes away, through its request's
	// context, or when the response body stalls.
	ctx, cancel := context.WithCancelCause(r.Context())
	defer cancel(nil)
	var connected atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) { connected.Store(true) },
	})

	var failure error
	proxy := &httputil.ReverseProxy{
		Rewrite:   func(pr *httputil.ProxyRequest) { rewriteToUpstream(pr, b.dial) },
		Transport: p.transport,
		ErrorLog:  p.errorLog,
		ModifyResponse: func(res *http.Response) error {
			// Every protocol switch is refused at ingress, so an upstream that
			// switches anyway is not followed into a tunnel. The proxy closes
			// the response, and with it the connection.
			if res.StatusCode == http.StatusSwitchingProtocols {
				return errProtocolSwitched
			}
			// Only the gateway assigns the response's correlation reference.
			res.Header.Del("X-Request-Id")
			res.Header.Add("Via", via(res.ProtoMajor, res.ProtoMinor))
			res.Body = newIdleBody(res.Body, p.readIdle, func() { cancel(errResponseStalled) })
			return nil
		},
		// The enforcement handler answers the client; only the cause is kept.
		ErrorHandler: func(_ http.ResponseWriter, _ *http.Request, err error) { failure = err },
	}
	proxy.ServeHTTP(w, r.WithContext(ctx))
	if failure == nil {
		return nil
	}
	return exchangeFailure(failure, connected.Load(), r.Context().Err() != nil)
}

// exchangeFailure classifies an exchange that failed before any response by
// what is known about delivery and, for the gateway's answer, whether the
// upstream timed out.
func exchangeFailure(err error, connected, clientGone bool) error {
	switch {
	case !connected:
		return fmt.Errorf("%w: %w", errNotSent, err)
	case clientGone:
		return fmt.Errorf("%w: %w", errClientGone, err)
	}
	// Once connected, only the response-header timeout ends the exchange
	// with a timeout error.
	if netErr, ok := errors.AsType[net.Error](err); ok && netErr.Timeout() {
		return fmt.Errorf("%w: %w: %w", errUpstreamTimeout, errMaybeSent, err)
	}
	return fmt.Errorf("%w: %w", errMaybeSent, err)
}

// rewriteToUpstream addresses the outbound request to dial and sets the
// gateway's forwarding metadata. The reverse proxy calls it after removing
// the hop-by-hop fields, including every field the client named in
// Connection, and the client's Forwarded and X-Forwarded-* fields, so the
// client can neither remove nor supply anything set here.
func rewriteToUpstream(pr *httputil.ProxyRequest, dial string) {
	in := pr.In.URL
	// The client's target as admitted: the path in its received encoding and
	// the query unparsed, unparsable parameters included. The gateway does not
	// interpret the query, so it cannot read it differently from the upstream.
	// Only the address changes: the outbound request keeps the inbound Host,
	// which reaches the upstream but never chooses it.
	pr.Out.URL = &url.URL{
		Scheme:     "http",
		Host:       dial,
		Path:       in.Path,
		RawPath:    in.RawPath,
		RawQuery:   in.RawQuery,
		ForceQuery: in.ForceQuery,
	}
	// The direct peer's address, the inbound Host and "http".
	pr.SetXForwarded()
	pr.Out.Header.Add("Via", via(pr.In.ProtoMajor, pr.In.ProtoMinor))
}

// via is the gateway's Via field value for a message received with the given
// HTTP version (RFC 9110, section 7.6.3).
func via(major, minor int) string {
	return strconv.Itoa(major) + "." + strconv.Itoa(minor) + " gateway"
}

// idleBody is an upstream response body each of whose reads may wait at most
// idle for data. A read that waits longer calls stall, which ends the
// exchange: the transport closes the connection and the blocked read fails.
// One timer serves every read; it runs only while a read is waiting.
type idleBody struct {
	io.ReadCloser
	idle  time.Duration
	timer *time.Timer
}

func newIdleBody(body io.ReadCloser, idle time.Duration, stall func()) *idleBody {
	timer := time.AfterFunc(idle, stall)
	timer.Stop()
	return &idleBody{ReadCloser: body, idle: idle, timer: timer}
}

func (b *idleBody) Read(p []byte) (int, error) {
	b.timer.Reset(b.idle)
	n, err := b.ReadCloser.Read(p)
	b.timer.Stop()
	return n, err
}
