// Enforcement handler helpers, admission and audit gate.

package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Alex-Zechariah-02/apiward/internal/audit"
	"github.com/Alex-Zechariah-02/apiward/internal/forward"
	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

// These tests run the enforcement handler under a plain net/http server in
// the test process, with no gateway process. Its forwarder is an in-memory
// stand-in for the reverse proxy that records what it is handed;
// the deployment's real receivers stay connected to nothing, so any request
// reaching them would show a forwarding path outside the handler.

// handedRequest is one admitted request as the forwarder received it.
type handedRequest struct {
	bound  bool // the request carried a trusted binding
	dial   string
	path   string // the bound path the request is forwarded with
	method string
	target string
	host   string
	header http.Header
	body   string
}

// memoryForwarder records every request it is handed and answers with
// respond, or with 200 "forwarded" when respond is nil.
type memoryForwarder struct {
	mu      sync.Mutex
	got     []handedRequest
	respond func(w http.ResponseWriter, r *http.Request) error
}

func (f *memoryForwarder) forward(w http.ResponseWriter, r *http.Request) error {
	dial, path, bound := forward.Bound(r.Context())
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.got = append(f.got, handedRequest{
		bound:  bound,
		dial:   dial,
		path:   path,
		method: r.Method,
		target: r.RequestURI,
		host:   r.Host,
		header: r.Header.Clone(),
		body:   string(body),
	})
	respond := f.respond
	f.mu.Unlock()
	if respond != nil {
		return respond(w, r)
	}
	io.WriteString(w, "forwarded")
	return nil
}

func (f *memoryForwarder) handed() []handedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]handedRequest(nil), f.got...)
}

func (f *memoryForwarder) setRespond(respond func(w http.ResponseWriter, r *http.Request) error) {
	f.mu.Lock()
	f.respond = respond
	f.mu.Unlock()
}

// serveEnforcement seeds e's database with policy, owns its controller and
// serves the enforcement handler, with the global body ceiling maxBodyBytes
// (0: the default), at e.dataAddr, where the harness request helpers send.
func (e *env) serveEnforcement(policy policy.Policy, maxBodyBytes int64) (*Controller, *Handler, *memoryForwarder) {
	e.t.Helper()
	seed(e.t, e.database, policy, seedOptions{})
	c, _ := e.own(e.controllerConfig())
	f := &memoryForwarder{}
	h, err := newHandler(c, maxBodyBytes, f.forward)
	if err != nil {
		e.t.Fatal(err)
	}
	server := httptest.NewServer(h)
	e.t.Cleanup(server.Close)
	e.dataAddr = server.Listener.Addr().String()
	return c, h, f
}

// eventOf returns the last event of the request with the given identifier,
// waiting for want events of it.
func (e *env) eventOf(requestID string, want int) map[string]any {
	e.t.Helper()
	events := e.requestEvents(requestID, want)
	if len(events) != want {
		e.t.Fatalf("request %s has %d events, want %d: %v", requestID, len(events), want, events)
	}
	return events[want-1]
}

// admittedStages is the stage map of a forwarded request to an operation
// without a rate rule, body or header schema.
func admittedStages(forward string) map[string]any {
	stages := expectedStages([]string{"INGRESS", "CAPTURE", "CANONICAL", "ROUTE", "ACCESS", "JWT", "RBAC", "BODY", "AUDIT", "BIND"}, "FORWARD", forward)
	stages["RATE"] = audit.StageNotApplicable
	stages["VALIDATION"] = audit.StageNotApplicable
	return stages
}

// The handler needs a controller and a forwarder; a ceiling of zero selects
// the default.
func TestNewHandlerRefusesIncompleteSetup(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.policy(e.a, false), seedOptions{})
	c, _ := e.own(e.controllerConfig())
	forward := (&memoryForwarder{}).forward
	if h, err := newHandler(nil, 0, forward); h != nil || err == nil {
		t.Fatalf("without a controller: %v, %v", h, err)
	}
	if h, err := newHandler(c, 0, nil); h != nil || err == nil {
		t.Fatalf("without a forwarder: %v, %v", h, err)
	}
	h, err := newHandler(c, 0, forward)
	if err != nil || h.maxBodyBytes != defaultMaxBodyBytes {
		t.Fatalf("default ceiling: %v, %v", h, err)
	}
}

// Under a plain net/http server an admitted request reaches the forwarder
// once, with the trusted binding of its captured generation, the gateway's
// own metadata and no credential, after a durable admission record; no real
// upstream is contacted.
func TestEnforcementHandlerAdmits(t *testing.T) {
	e := newEnv(t)
	c, _, f := e.serveEnforcement(e.protectedPolicy(), 0)
	g := c.Current()
	admittedFirst := false
	f.setRespond(func(w http.ResponseWriter, r *http.Request) error {
		// The admission record is durable before the forwarder runs.
		id := r.Header.Get("X-Request-Id")
		for _, ev := range e.auditEvents() {
			admittedFirst = admittedFirst || (ev["kind"] == "request.admitted" && ev["request_id"] == id)
		}
		w.Header().Set("X-Upstream", "memory")
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, "created")
		return nil
	})

	token := issuer.tokenFor(t, "alice")
	resp, body := e.exchange(http.MethodGet, "/a/x?q=1", "", "", "Authorization", "Bearer "+token,
		"X-Request-Id", "forged", "X-Gateway-Subject", "admin", "X-Gateway-Roles", "ADMIN")
	if resp.StatusCode != http.StatusCreated || body != "created" || resp.Header.Get("X-Upstream") != "memory" {
		t.Fatalf("admitted request: %d %q %v", resp.StatusCode, body, resp.Header)
	}
	got := f.handed()
	if len(got) != 1 {
		t.Fatalf("%d requests handed on, want 1", len(got))
	}
	h := got[0]
	switch {
	case !h.bound || h.dial != e.a.addr:
		t.Fatalf("binding %v %q, want %q", h.bound, h.dial, e.a.addr)
	case h.method != http.MethodGet || h.target != "/a/x?q=1":
		t.Fatalf("handed %s %s", h.method, h.target)
	case h.header.Get("X-Gateway-Generation") != g.ID:
		t.Fatalf("generation %q, want %q", h.header.Get("X-Gateway-Generation"), g.ID)
	case h.header.Get("X-Gateway-Subject") != "alice" || h.header.Get("X-Gateway-Roles") != "WRITER":
		t.Fatalf("identity headers %v", h.header)
	case h.header.Get("X-Request-Id") == "" || h.header.Get("X-Request-Id") == "forged":
		t.Fatalf("request identifier %q", h.header.Get("X-Request-Id"))
	case h.header.Get("Authorization") != "":
		t.Fatal("the credential was handed on")
	case !admittedFirst:
		t.Fatal("the admission record was not durable before forwarding")
	}
	ev := e.eventOf(h.header.Get("X-Request-Id"), 2)
	if ev["kind"] != "request.forwarded" || ev["decision"] != "forwarded" || ev["status"] != float64(201) ||
		ev["generation"] != g.ID || ev["upstream"] != "A" || ev["subject"] != "alice" ||
		!sameStages(ev["stages"], admittedStages(audit.StagePass)) {
		t.Fatalf("outcome event %v", ev)
	}
	if audit := e.auditEvents(); strings.Contains(fmt.Sprint(audit), token[:40]) || strings.Contains(fmt.Sprint(audit), "q=1") {
		t.Fatal("the audit log holds the token or the query")
	}
	e.assertReceipts(0, 0, 0)
}

// When the audit log cannot durably record an admission, nothing is
// forwarded: the request is refused at the AUDIT stage, after every other
// stage passed, and the refusal it cannot record is counted as lost.
func TestEnforcementAuditFailureBlocksForwarding(t *testing.T) {
	e := newEnv(t)
	c, _, f := e.serveEnforcement(e.protectedPolicy(), 0)
	if code, _ := e.get("/a/x", "Authorization", "Bearer "+issuer.tokenFor(t, "alice")); code != http.StatusOK {
		t.Fatalf("before the failure: %d", code)
	}
	// The state a failed write or sync leaves the log in.
	c.audit.Fail(errors.New("injected write failure"))
	lost := c.audit.Lost()
	code, body := e.get("/a/x", "Authorization", "Bearer "+issuer.tokenFor(t, "alice"))
	var reply struct {
		Error     string `json:"error"`
		RequestID string `json:"request_id"`
	}
	json.Unmarshal([]byte(body), &reply)
	if code != http.StatusServiceUnavailable || reply.Error != "gateway_unavailable" || reply.RequestID == "" {
		t.Fatalf("with a failed audit log: %d %s", code, body)
	}
	if n := len(f.handed()); n != 1 {
		t.Fatalf("%d requests handed on, want 1", n)
	}
	if got := c.audit.Lost(); got != lost+1 {
		t.Fatalf("lost events %d, want %d", got, lost+1)
	}
	// A refusal before the AUDIT stage needs no acknowledgement and is
	// answered as before.
	if code, _ := e.get("/a/x"); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated with a failed audit log: %d", code)
	}
	e.assertReceipts(0, 0, 0)
}
