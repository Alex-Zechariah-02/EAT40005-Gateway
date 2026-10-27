package gateway

import (
	"net/http"
	"slices"
	"sync"
	"time"
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

func (o *observations) record(event Event) {
	view := requestEvent{
		RequestID: event.RequestID, Time: event.Time, Method: event.Method, Path: event.Path,
		Operation: optionalString(event.Operation), API: optionalString(event.API), Subject: optionalString(event.Subject),
		Roles: slices.Clone(event.Roles), ClientAddress: event.ClientAddress, Status: event.Status,
		Code: optionalString(event.Error), Generation: event.Generation, DurationMS: event.DurationMS,
	}
	if view.Roles == nil {
		view.Roles = []string{}
	}
	if len(view.Path) > maxLoggedPath {
		view.Path = view.Path[:maxLoggedPath]
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
	o.mu.Lock()
	defer o.mu.Unlock()
	o.metrics.record(view)
	o.next++
	asserted := append(slices.Clone(view.Roles), event.IgnoredRoles...)
	slices.Sort(asserted)
	request := observedRequest{requestEvent: view, sequence: o.next, assertedRoles: asserted, identityKnown: event.identityKnown}
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
func (g *Generation) previewDecision(event observedRequest) (decision, code, operation string, known bool) {
	if !isCanonical(event.Path) {
		return "refused", "bad_request", "", true
	}
	target, rule := g.match(event.Path, event.Method)
	if target == nil {
		return "refused", "not_found", "", true
	}
	if rule == nil {
		return "refused", "method_not_allowed", "", true
	}
	if !rule.permitted {
		return "refused", "forbidden", rule.operation, true
	}
	if !rule.public {
		if !event.identityKnown {
			return "", "", rule.operation, false
		}
		if event.Subject == nil {
			return "refused", "unauthenticated", rule.operation, true
		}
		if event.Stage == "blocked" {
			return "refused", "account_blocked", rule.operation, true
		}
		if _, _, reason := g.authorise(event.assertedRoles, rule); reason != "" {
			return "refused", "forbidden", rule.operation, true
		}
	}
	return "allowed", "", rule.operation, true
}

// traffic failures are refusals from the caller's perspective, while request
// events retain the distinction between refusal and an unknown upstream result.
func wasAllowed(event requestEvent) bool {
	return event.Decision == "allowed" && event.Status >= http.StatusOK
}
