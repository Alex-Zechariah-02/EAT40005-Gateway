// Data-plane handler and its request pipeline.

package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"time"
)

const defaultMaxBodyBytes = 16 << 10

// Handler enforces the published policy on every request and, only when every
// stage passes, binds the permitted upstream and hands the request to its
// forwarder. Any other outcome is answered here and never forwarded. It is an
// http.Handler, so any net/http server can run it.
type Handler struct {
	controller *Controller
	// maxBodyBytes is the global request body ceiling. A larger declared
	// length is refused before any policy work, and the body read for an
	// operation is bounded by it as well as by the operation's own ceiling.
	maxBodyBytes int64
	forward      forwardFunc
}

// forwardFunc sends an admitted request to the upstream bound in its context
// and copies the response to w. It returns an error when the exchange failed;
// the handler then answers with the status forwardFailure gives it, unless a
// response was already started. A response it cannot complete after it
// started is aborted by panicking with http.ErrAbortHandler, as net/http's
// reverse proxy does.
type forwardFunc func(w http.ResponseWriter, r *http.Request) error

// newHandler returns the enforcement handler for the policy c publishes. A
// maxBodyBytes of zero or less selects the default ceiling. forward is called
// only for admitted requests.
func newHandler(c *Controller, maxBodyBytes int64, forward forwardFunc) (*Handler, error) {
	if c == nil || forward == nil {
		return nil, errors.New("the enforcement handler needs a controller and a forwarder")
	}
	if maxBodyBytes <= 0 {
		maxBodyBytes = defaultMaxBodyBytes
	}
	return &Handler{controller: c, maxBodyBytes: maxBodyBytes, forward: forward}, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := &request{
		w:            w,
		audit:        h.controller.audit,
		observations: h.controller.observations,
		started:      time.Now(),
		event:        Event{RequestID: newRequestID(), Time: time.Now().UTC(), Method: r.Method, Path: r.URL.Path},
		bodyPending:  r.ContentLength != 0 || len(r.TransferEncoding) > 0,
	}
	w.Header().Set("X-Request-Id", q.event.RequestID)
	if peer, err := netip.ParseAddrPort(r.RemoteAddr); err == nil {
		q.event.ClientAddress = peer.Addr().Unmap().String()
	}
	// Keep credentials only for authentication; discard client identity metadata
	// before any ingress refusal as well as before forwarding.
	credentials := r.Header.Values("Authorization")
	q.event.identityKnown = len(credentials) == 0
	stripUntrustedHeaders(r.Header)

	// Ingress: only origin-form targets, no tunnelling, bounded declared size.
	// A protocol upgrade would turn an admitted request into a raw stream the
	// policy never sees. The proxy removes every header named in Connection,
	// so naming a gateway header there would strip it after it is set here.
	if !strings.HasPrefix(r.RequestURI, "/") ||
		r.Method == http.MethodConnect ||
		r.Header.Get("Upgrade") != "" ||
		slices.ContainsFunc(connectionNames(r.Header), isGatewayHeader) {
		q.reject(stageIngress, http.StatusBadRequest, "bad_request")
		return
	}
	if r.ContentLength > h.maxBodyBytes {
		q.reject(stageIngress, http.StatusRequestEntityTooLarge, "payload_too_large")
		return
	}
	q.pass(stageIngress)

	// Capture: the single generation used for every later decision.
	g := h.controller.Current()
	if g == nil {
		q.reject(stageCapture, http.StatusServiceUnavailable, "gateway_unavailable")
		return
	}
	// A generation past its earliest deadline (a provisional base's or an
	// override's) admits nothing, even if the controller's timer is late; the
	// controller is asked to end what expired now.
	if g.expired(trustedNow()) {
		h.controller.deadlinePassed(g)
		q.reject(stageCapture, http.StatusServiceUnavailable, "gateway_unavailable")
		return
	}
	q.event.Generation, q.event.Profile = g.ID, g.Profile
	q.pass(stageCapture)
	signalCapture(r.URL.Path, g.ID)

	path, ok := CanonicalPath(r)
	if !ok {
		q.reject(stageCanonical, http.StatusBadRequest, "bad_request")
		return
	}
	q.pass(stageCanonical)

	t, rule := g.match(path, r.Method)
	if t == nil {
		q.reject(stageRoute, http.StatusNotFound, "not_found")
		return
	}
	if rule == nil {
		w.Header().Set("Allow", t.allow)
		q.reject(stageRoute, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	q.event.Operation, q.event.API, q.event.Upstream = rule.operation, rule.api, rule.upstream
	q.pass(stageRoute)

	if !rule.permitted {
		q.reject(stageAccess, http.StatusForbidden, "forbidden")
		return
	}
	q.pass(stageAccess)

	// Authentication and authorisation apply to every operation not declared
	// public. Roles come only from verified claims and are filtered through
	// the declared roles of the one captured generation.
	var roles []string
	if rule.public {
		q.event.Stages[stageJWT] = stageNotApplicable
		q.event.Stages[stageRBAC] = stageNotApplicable
	} else {
		q.event.identityKnown = true
		token, reason := bearerToken(credentials)
		var verified verifiedToken
		if reason == "" {
			verified, reason = h.controller.verifier.Load().verify(token, time.Now())
			q.event.Subject = verified.subject
		}
		if reason != "" {
			q.event.Reason = reason
			w.Header().Set("WWW-Authenticate", "Bearer")
			q.reject(stageJWT, http.StatusUnauthorized, "unauthenticated")
			return
		}
		q.pass(stageJWT)
		roles, q.event.IgnoredRoles, reason = g.authorise(verified.roles, rule)
		q.event.Roles = roles
		blocked := h.controller.blocked.Load()
		if blocked == nil {
			q.reject(stageRBAC, http.StatusServiceUnavailable, "gateway_unavailable")
			return
		}
		if _, found := (*blocked)[q.event.Subject]; found {
			q.event.Reason = "account_blocked"
			q.reject(stageRBAC, http.StatusForbidden, "account_blocked")
			return
		}

		if reason != "" {
			q.event.Reason = reason
			q.reject(stageRBAC, http.StatusForbidden, "forbidden")
			return
		}
		q.pass(stageRBAC)
	}

	// Rate limiting uses either the verified subject or the direct socket
	// peer. Client forwarding headers never choose an address counter.
	if rule.rate == nil {
		q.event.Stages[stageRate] = stageNotApplicable
	} else {
		key := counterKey{operation: rule.operation, keyType: rule.rateKey, key: q.event.Subject}
		if rule.rateKey == rateKeyAddress {
			if q.event.ClientAddress == "" {
				q.event.Reason = "invalid_peer_address"
				q.reject(stageRate, http.StatusBadRequest, "bad_request")
				return
			}
			key.key = q.event.ClientAddress
		}
		switch h.controller.limiter.admit(key, rule.rate, h.controller.clock.now()) {
		case rateExceeded:
			q.event.Reason = "quota_exceeded"
			q.reject(stageRate, http.StatusTooManyRequests, "rate_limited")
			return
		case rateNoCapacity:
			q.event.Reason = "rate_capacity"
			q.reject(stageRate, http.StatusServiceUnavailable, "gateway_unavailable")
			return
		}
		q.pass(stageRate)
	}

	document, refused := readBody(r, rule.body, h.maxBodyBytes)
	if refused != nil {
		q.event.Reason = refused.reason
		q.reject(stageBody, refused.status, refused.code)
		return
	}
	q.bodyPending = false
	q.pass(stageBody)

	if rule.body == nil && rule.headers == nil {
		q.event.Stages[stageValidation] = stageNotApplicable
	} else {
		if reason := validateRequest(r.Header, document, rule); reason != "" {
			q.event.Reason = reason
			q.reject(stageValidation, http.StatusBadRequest, "bad_request")
			return
		}
		q.pass(stageValidation)
	}

	// Admission requires a durable audit record before anything is forwarded.
	q.event.Kind, q.event.Stage, q.event.Decision = "request.admitted", stageNames[stageAudit], "admit"
	ctx, cancel := context.WithTimeout(r.Context(), h.controller.config.AuditTimeout)
	err := q.audit.Commit(ctx, q.event)
	cancel()
	if err != nil {
		q.reject(stageAudit, http.StatusServiceUnavailable, "gateway_unavailable")
		return
	}
	q.pass(stageAudit)

	r.Header.Set("X-Request-Id", q.event.RequestID)
	r.Header.Set("X-Gateway-Generation", g.ID)
	if !rule.public {
		// The upstream can rely on these for its own object-level rules: client
		// copies were removed at ingress.
		r.Header.Set("X-Gateway-Subject", q.event.Subject)
		r.Header.Set("X-Gateway-Roles", strings.Join(roles, ","))
	}
	r = r.WithContext(context.WithValue(r.Context(), bindingKey{}, binding{dial: rule.dial}))
	q.pass(stageBind)

	recorder := &statusRecorder{ResponseWriter: w}
	finished := false
	defer func() {
		if !finished {
			// The forwarder aborts a response it cannot complete, such as a
			// truncated upstream body, by panicking with http.ErrAbortHandler
			// after the status was sent. The outcome is recorded here; the
			// panic is not recovered, so the server still closes the
			// connection and the client sees the response is incomplete.
			q.forwarded(recorder.status, "upstream_failure", "upstream_incomplete")
		}
	}()
	forwardErr := h.forward(recorder, r)
	finished = true
	if forwardErr != nil {
		status, code := forwardFailure(forwardErr)
		if !recorder.wrote {
			writeError(w, status, code, q.event.RequestID)
		}
		q.event.Reason = failureReason(forwardErr)
		q.forwarded(status, "upstream_failure", code)
		return
	}
	q.forwarded(recorder.status, "forwarded", "")
}
