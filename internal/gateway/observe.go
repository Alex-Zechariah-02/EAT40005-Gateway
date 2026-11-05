package gateway

import (
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/audit"
	"github.com/Alex-Zechariah-02/apiward/internal/compile"
	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

const recentEventLimit = 10000
const streamQueueSize = 128
const maxEventStreams = 8

// requestEvent is the explicit, secret-free Admin API projection. Private audit
// envelopes and their future fields are never serialized to viewers.
type requestEvent struct {
	RequestID     string    `json:"request_id"`
	Time          time.Time `json:"time"`
	Method        string    `json:"method"`
	Path          string    `json:"path"`
	Operation     *string   `json:"operation"`
	API           *string   `json:"api"`
	Subject       *string   `json:"subject"`
	Roles         []string  `json:"roles"`
	ClientAddress string    `json:"client_address"`
	Decision      string    `json:"decision"`
	Status        int       `json:"status"`
	Stage         string    `json:"stage"`
	Code          *string   `json:"code"`
	Generation    string    `json:"generation"`
	DurationMS    float64   `json:"duration_ms"`
}

type observedRequest struct {
	requestEvent
	sequence      uint64
	assertedRoles []string // verified assertions, including roles dropped by the active policy
	identityKnown bool     // evaluated authentication, or no credential was supplied
	// routingPath is separate from the displayed path. One byte beyond the
	// longest target distinguishes an exact match from a longer request; prefix
	// matching needs no more. Empty means canonicalization did not pass.
	routingPath string
}

type eventViewer struct {
	events  chan observedRequest
	dropped uint64 // guarded by observations.mu
}

// observations owns a fixed-size ring and bounded, nonblocking subscriptions.
// It has no authority over admission, policy, durable audit or upstream writes.
type observations struct {
	mu      sync.Mutex
	ring    [recentEventLimit]observedRequest
	count   int
	next    uint64
	viewers map[*eventViewer]bool
	dropped uint64
	done    chan struct{}
	closed  bool
	metrics requestMetrics
}

func newObservations() *observations {
	return &observations{viewers: map[*eventViewer]bool{}, done: make(chan struct{})}
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// requestView is the secret-free view of a request's final audit event, shared
// by the live views and the security log search so both show the same fields.
func requestView(event audit.Event) requestEvent {
	view := requestEvent{
		RequestID: event.RequestID, Time: event.Time, Method: event.Method,
		Path:      strings.Clone(event.Path[:min(len(event.Path), audit.MaxLoggedPath)]),
		Operation: optionalString(event.Operation), API: optionalString(event.API), Subject: optionalString(event.Subject),
		Roles: slices.Clone(event.Roles), ClientAddress: event.ClientAddress, Status: event.Status,
		Code: optionalString(event.Error), Generation: event.Generation, DurationMS: event.DurationMS,
	}
	if view.Roles == nil {
		view.Roles = []string{}
	}
	switch event.Decision {
	case "forwarded":
		view.Decision = "allowed"
	case "upstream_failure":
		view.Decision = "failed"
	default:
		view.Decision = "refused"
	}
	switch event.Stage {
	case "FORWARD", "BIND":
		view.Stage = "forward"
	case "ACCESS":
		view.Stage = "access"
	case "JWT":
		view.Stage = "token"
	case "RBAC":
		view.Stage = "permit"
		if event.Error == "account_blocked" {
			view.Stage = "blocked"
		}
	case "RATE":
		view.Stage = "rate"
	case "BODY", "VALIDATION", "INGRESS", "CANONICAL":
		view.Stage = "validation"
	case "AUDIT", "CAPTURE":
		view.Stage = "record"
	default:
		view.Stage = "route"
	}
	return view
}

func (o *observations) record(event audit.Event) {
	view := requestView(event)
	var routingPath string
	if event.Stages[audit.StageCanonical] == audit.StagePass {
		routingPath = strings.Clone(event.Path[:min(len(event.Path), policy.MaxTargetLength+1)])
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.metrics.record(view)
	o.next++
	asserted := append(slices.Clone(view.Roles), event.IgnoredRoles...)
	slices.Sort(asserted)
	request := observedRequest{requestEvent: view, sequence: o.next, assertedRoles: asserted, identityKnown: event.IdentityKnown(), routingPath: routingPath}
	o.ring[(o.next-1)%recentEventLimit] = request
	if o.count < recentEventLimit {
		o.count++
	}
	for viewer := range o.viewers {
		select {
		case viewer.events <- request:
		default:
			viewer.dropped++
			o.dropped++
		}
	}
}

// recent returns, newest first, the retained requests that keep accepts,
// stopping once it has limit of them (0: no limit). Only returned requests
// are copied, so a reader that wants a short window or a few matches does
// not copy the whole ring while holding the lock the request path records
// under. keep runs under that lock and must not block.
func (o *observations) recent(keep func(*observedRequest) bool, limit int) []observedRequest {
	o.mu.Lock()
	defer o.mu.Unlock()
	var result []observedRequest
	for i := range o.count {
		request := &o.ring[(o.next-1-uint64(i))%recentEventLimit]
		if !keep(request) {
			continue
		}
		result = append(result, *request)
		if len(result) == limit {
			break
		}
	}
	return result
}

// retainedSince returns the time of the oldest request still kept and
// whether older requests have already been dropped from the ring.
func (o *observations) retainedSince() (time.Time, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.count == 0 {
		return time.Time{}, false
	}
	return o.ring[(o.next-uint64(o.count))%recentEventLimit].Time, o.next > recentEventLimit
}

func (o *observations) snapshot() []observedRequest {
	o.mu.Lock()
	defer o.mu.Unlock()
	result := make([]observedRequest, o.count)
	for i := range o.count {
		result[i] = o.ring[(o.next-1-uint64(i))%recentEventLimit]
	}
	return result
}

func (o *observations) subscribe() *eventViewer {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed || len(o.viewers) >= maxEventStreams {
		return nil
	}
	viewer := &eventViewer{events: make(chan observedRequest, streamQueueSize)}
	o.viewers[viewer] = true
	return viewer
}

func (o *observations) unsubscribe(viewer *eventViewer) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.viewers, viewer)
}

func (o *observations) takeDropped(viewer *eventViewer) uint64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	count := viewer.dropped
	viewer.dropped = 0
	return count
}

func (o *observations) close() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.closed {
		o.closed = true
		close(o.done)
	}
}

// previewDecision shares canonical matching and permit evaluation with the
// request pipeline. It deliberately does not touch counters or validate bodies.
func previewDecision(g *compile.Generation, event observedRequest) (decision, code, operation string, known bool) {
	if event.routingPath == "" {
		return "refused", "bad_request", "", true
	}
	target, rule := g.Match(event.routingPath, event.Method)
	if target == nil {
		return "refused", "not_found", "", true
	}
	if rule == nil {
		return "refused", "method_not_allowed", "", true
	}
	if !rule.Permitted() {
		return "refused", "forbidden", rule.Operation(), true
	}
	if !rule.Public() {
		if !event.identityKnown {
			return "", "", rule.Operation(), false
		}
		if event.Subject == nil {
			return "refused", "unauthenticated", rule.Operation(), true
		}
		if event.Stage == "blocked" {
			return "refused", "account_blocked", rule.Operation(), true
		}
		if _, _, reason := g.Authorise(event.assertedRoles, rule); reason != "" {
			return "refused", "forbidden", rule.Operation(), true
		}
	}
	return "allowed", "", rule.Operation(), true
}

// traffic failures are refusals from the caller's perspective, while request
// events retain the distinction between refusal and an unknown upstream result.
func wasAllowed(event requestEvent) bool {
	return event.Decision == "allowed" && event.Status >= http.StatusOK
}
