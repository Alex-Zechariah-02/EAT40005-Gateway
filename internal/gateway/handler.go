package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
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

// Forwarding failures that decide the gateway's own answer; any other error
// means the upstream could not be reached or failed.
var (
	errNoUpstream      = errors.New("no upstream for the request")
	errUpstreamTimeout = errors.New("upstream timed out")
)

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

// binding is the trusted upstream decision carried in the request context.
// Only this package can create it; no header or placeholder can.
type binding struct{ dial string }

type bindingKey struct{}

// request tracks one request through the stages for its security event.
type request struct {
	event Event
	w     http.ResponseWriter
	audit *auditLog
	// bodyPending is true while a declared request body has not been
	// consumed by validation.
	bodyPending bool
}

func (q *request) pass(stage int) { q.event.Stages[stage] = stagePass }

func (q *request) reject(stage int, status int, code string) {
	q.event.Kind, q.event.Stage, q.event.Decision = "request.rejected", stageNames[stage], "reject"
	q.event.Stages[stage] = stageReject
	q.event.Status, q.event.Error = status, code
	q.audit.Record(q.event)
	if q.bodyPending {
		// Closing the connection stops net/http from reading the unread body
		// before it writes the rejection, which a client that declares a body
		// and never sends it could otherwise hold open.
		q.w.Header().Set("Connection", "close")
	}
	writeError(q.w, status, code, q.event.RequestID)
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := &request{
		w:           w,
		audit:       h.controller.audit,
		event:       Event{RequestID: newRequestID(), Method: r.Method, Path: r.URL.Path},
		bodyPending: r.ContentLength != 0 || len(r.TransferEncoding) > 0,
	}

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
	// The credential is kept only for the authentication stage; the header is
	// removed with the others, so it is never forwarded.
	credentials := r.Header.Values("Authorization")
	stripUntrustedHeaders(r.Header)
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
	// public. Roles come only from the captured generation, never from the
	// token or the request.
	var roles []string
	if rule.public {
		q.event.Stages[stageJWT] = stageNotApplicable
		q.event.Stages[stageRBAC] = stageNotApplicable
	} else {
		token, reason := bearerToken(credentials)
		if reason == "" {
			q.event.Subject, reason = h.controller.verifier.verify(token, time.Now())
		}
		if reason != "" {
			q.event.Reason = reason
			w.Header().Set("WWW-Authenticate", "Bearer")
			q.reject(stageJWT, http.StatusUnauthorized, "unauthenticated")
			return
		}
		q.pass(stageJWT)

		roles, reason = g.authorise(q.event.Subject, rule)
		if reason != "" {
			q.event.Reason = reason
			q.reject(stageRBAC, http.StatusForbidden, "forbidden")
			return
		}
		q.pass(stageRBAC)
	}

	// Rate limiting counts only requests whose subject was verified and
	// authorised above. The counter key is that subject, never a request value.
	if rule.rate == nil {
		q.event.Stages[stageRate] = stageNotApplicable
	} else {
		key := counterKey{operation: rule.operation, keyType: rateKeySubject, key: q.event.Subject}
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

// forwarded records the outcome of a request that was handed to the
// forwarder.
func (q *request) forwarded(status int, decision, code string) {
	q.event.Kind, q.event.Stage = "request.forwarded", stageNames[stageForward]
	q.event.Stages[stageForward] = stagePass
	if decision != "forwarded" {
		q.event.Stages[stageForward] = stageFail
	}
	q.event.Decision, q.event.Status, q.event.Error = decision, status, code
	q.audit.Record(q.event)
}

// isGatewayHeader reports whether only the gateway may set the named header.
func isGatewayHeader(name string) bool {
	name = http.CanonicalHeaderKey(name)
	return name == "X-Request-Id" || strings.HasPrefix(name, "X-Gateway-")
}

// connectionNames returns, in canonical form, the field names that the
// Connection fields of header nominate; the reverse proxy removes each of
// them before forwarding (RFC 9110, section 7.6.1). It reads every field line
// and every comma-separated element, ignoring empty ones. Trimming white space
// here covers at least what the proxy trims, so no name the proxy removes is
// missed. An element that is not a field name, such as a quoted one, comes
// back unchanged and matches no field, as it removes none at the proxy.
func connectionNames(header http.Header) []string {
	var names []string
	for _, value := range header.Values("Connection") {
		for element := range strings.SplitSeq(value, ",") {
			if name := strings.TrimSpace(element); name != "" {
				names = append(names, http.CanonicalHeaderKey(name))
			}
		}
	}
	return names
}

// stripUntrustedHeaders removes headers that only the gateway may set, and the
// client credential, which upstreams do not receive. It also removes every
// header whose name holds '_' or '.': an upstream that maps header names to
// CGI-style variables folds those characters and '-' alike, so such a name
// could stand in for another header, a gateway header included. No header
// schema may declare such a name.
func stripUntrustedHeaders(header http.Header) {
	for name := range header {
		if isGatewayHeader(name) || strings.ContainsAny(name, "_.") {
			delete(header, name)
		}
	}
	header.Del("Authorization")
}

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

func writeError(w http.ResponseWriter, status int, code, requestID string) {
	writeJSON(w, status, map[string]string{"error": code, "request_id": requestID})
}

func newRequestID() string {
	id := make([]byte, 16)
	rand.Read(id)
	return hex.EncodeToString(id)
}

// statusRecorder notes the final status the forwarder wrote, while passing
// everything through unchanged.
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusRecorder) WriteHeader(status int) {
	// Informational (1xx) responses precede the final status.
	if !s.wrote && status >= 200 {
		s.status, s.wrote = status, true
	}
	s.ResponseWriter.WriteHeader(status)
}

func (s *statusRecorder) Write(data []byte) (int, error) {
	if !s.wrote {
		s.status, s.wrote = http.StatusOK, true
	}
	return s.ResponseWriter.Write(data)
}

// Unwrap lets http.ResponseController reach the underlying writer, so a
// forwarder can flush a streamed response through the recorder.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// Interface guard
var _ http.Handler = (*Handler)(nil)
