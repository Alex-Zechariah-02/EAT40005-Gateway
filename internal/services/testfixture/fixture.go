package testfixture

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

const (
	maxBodyBytes      = 8 << 10
	maxReceipts       = 10000
	maxRecordedTarget = 512
	minSecretBytes    = 32
)

// Receipt is the fixture's own record of one request it received, written
// whether or not an operation matched.
type Receipt struct {
	Seq           int       `json:"seq"`
	Time          time.Time `json:"time"`
	RequestID     string    `json:"request_id"`
	Generation    string    `json:"generation"`
	Method        string    `json:"method"`
	Target        string    `json:"target"`
	Operation     string    `json:"operation"`
	Status        int       `json:"status"`
	Effect        string    `json:"effect,omitempty"`
	Authorization bool      `json:"authorization"`
	Subject       string    `json:"subject,omitempty"` // X-Gateway-Subject as received
	Roles         string    `json:"roles,omitempty"`   // X-Gateway-Roles as received
	BodyBytes     int       `json:"body_bytes"`
	BodySHA256    string    `json:"body_sha256,omitempty"`
}

// operation is one endpoint of a fixture API. Pattern segments written as
// "{id}" match exactly one non-empty path segment.
type operation struct {
	id      string
	method  string
	pattern string
	run     func(id string, r *http.Request, body []byte) result
}

// result is what an operation did: its response and a description of the
// state change it made, empty when it changed nothing.
type result struct {
	status int
	body   any
	effect string
}

// dataset is the synthetic state of one fixture.
type dataset interface {
	reset()
	snapshot() any
}

// Service is one running fixture API.
type Service struct {
	name       string
	version    string
	instance   string
	operations []operation
	data       dataset

	mu       sync.Mutex // guards data and receipts; operations run under it
	receipts []Receipt
}

// New returns the named fixture with its seed data loaded.
func New(name string) (*Service, error) {
	instance := make([]byte, 8)
	rand.Read(instance)
	s := &Service{name: name, instance: hex.EncodeToString(instance)}
	switch name {
	case "orders":
		d := &orders{}
		s.data, s.operations, s.version = d, d.operations(), ordersVersion
	case "inventory":
		d := &inventory{}
		s.data, s.operations, s.version = d, d.operations(), inventoryVersion
	case "reports":
		d := &reports{}
		s.data, s.operations, s.version = d, d.operations(), reportsVersion
	default:
		return nil, fmt.Errorf("unknown fixture %q", name)
	}
	s.data.reset()
	return s, nil
}

// ServeHTTP runs the matching operation and records the receipt. Every
// request is recorded unless the receipt log is full; then it is refused
// without a receipt. A missing receipt alone does not establish non-arrival.
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, readErr := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))

	s.mu.Lock()
	defer s.mu.Unlock()

	// Refuse rather than accept a request whose receipt cannot be kept.
	if len(s.receipts) >= maxReceipts {
		s.write(w, result{status: http.StatusServiceUnavailable, body: errorBody("receipts_full")})
		return
	}

	op, id := s.find(r.Method, r.URL.Path)
	var res result
	switch {
	case readErr != nil:
		res = result{status: http.StatusRequestEntityTooLarge, body: errorBody("payload_too_large")}
	case op == nil:
		res = result{status: http.StatusNotFound, body: errorBody("not_found")}
	default:
		res = op.run(id, r, body)
	}

	receipt := Receipt{
		Seq:           len(s.receipts) + 1,
		Time:          time.Now().UTC(),
		RequestID:     r.Header.Get("X-Request-Id"),
		Generation:    r.Header.Get("X-Gateway-Generation"),
		Method:        r.Method,
		Target:        r.RequestURI,
		Status:        res.status,
		Effect:        res.effect,
		Authorization: r.Header.Get("Authorization") != "",
		Subject:       r.Header.Get("X-Gateway-Subject"),
		Roles:         r.Header.Get("X-Gateway-Roles"),
		BodyBytes:     len(body),
	}
	if len(receipt.Target) > maxRecordedTarget {
		receipt.Target = receipt.Target[:maxRecordedTarget]
	}
	if len(receipt.Subject) > maxRecordedTarget {
		receipt.Subject = receipt.Subject[:maxRecordedTarget]
	}
	if len(receipt.Roles) > maxRecordedTarget {
		receipt.Roles = receipt.Roles[:maxRecordedTarget]
	}
	if op != nil {
		receipt.Operation = op.id
	}
	if len(body) > 0 {
		sum := sha256.Sum256(body)
		receipt.BodySHA256 = hex.EncodeToString(sum[:])
	}
	s.receipts = append(s.receipts, receipt)
	if res.effect != "" && dropAfterWrite(w) {
		return
	}
	s.write(w, res)
}

func (s *Service) find(method, path string) (*operation, string) {
	for i := range s.operations {
		op := &s.operations[i]
		if op.method != method {
			continue
		}
		if id, ok := matchPattern(op.pattern, path); ok {
			return op, id
		}
	}
	return nil, ""
}

// matchPattern compares path segment by segment. A "{id}" segment matches
// one non-empty segment, which is returned.
func matchPattern(pattern, path string) (string, bool) {
	want := strings.Split(pattern, "/")
	got := strings.Split(path, "/")
	if len(want) != len(got) {
		return "", false
	}
	id := ""
	for i := range want {
		if want[i] == "{id}" && got[i] != "" {
			id = got[i]
			continue
		}
		if want[i] != got[i] {
			return "", false
		}
	}
	return id, true
}

func (s *Service) write(w http.ResponseWriter, res result) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Service", s.name)
	w.Header().Set("X-Fixture-Instance", s.instance)
	w.WriteHeader(res.status)
	json.NewEncoder(w).Encode(res.body)
}

// State is the control view of a fixture's data.
type State struct {
	Service  string          `json:"service"`
	Version  string          `json:"version"`
	Instance string          `json:"instance"`
	SHA256   string          `json:"sha256"`
	Data     json.RawMessage `json:"data"`
}

func (s *Service) state() State {
	data, _ := json.Marshal(s.data.snapshot())
	sum := sha256.Sum256(data)
	return State{
		Service:  s.name,
		Version:  s.version,
		Instance: s.instance,
		SHA256:   hex.EncodeToString(sum[:]),
		Data:     data,
	}
}

// Control returns the privileged handler for receipts, state and reset. It
// must be served only on a listener that the gateway's clients cannot reach.
func (s *Service) Control(secret []byte) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /receipts", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		receipts := append([]Receipt{}, s.receipts...)
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"service": s.name, "instance": s.instance, "receipts": receipts})
	})
	mux.HandleFunc("GET /state", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		state := s.state()
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, state)
	})
	mux.HandleFunc("POST /reset", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.data.reset()
		s.receipts = nil
		state := s.state()
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, state)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Service", s.name)
		token, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !found || subtle.ConstantTimeCompare([]byte(token), secret) != 1 {
			writeJSON(w, http.StatusUnauthorized, errorBody("unauthenticated"))
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// Run serves the API and control listeners until ctx is cancelled. Both
// addresses must be literal loopback addresses, so neither is reachable from
// another host.
func Run(ctx context.Context, s *Service, listen, control string, secret []byte) error {
	if len(secret) < minSecretBytes {
		return fmt.Errorf("control secret must be at least %d bytes", minSecretBytes)
	}
	servers := []*http.Server{
		{Addr: listen, Handler: s},
		{Addr: control, Handler: s.Control(secret)},
	}
	listeners := make([]net.Listener, 0, len(servers))
	defer func() {
		for _, l := range listeners {
			l.Close()
		}
	}()
	for _, server := range servers {
		addr, err := netip.ParseAddrPort(server.Addr)
		if err != nil || !addr.Addr().IsLoopback() {
			return fmt.Errorf("address %q must be a literal loopback address", server.Addr)
		}
		listener, err := net.Listen("tcp", server.Addr)
		if err != nil {
			return err
		}
		listeners = append(listeners, listener)
		server.ReadHeaderTimeout = 5 * time.Second
		server.ReadTimeout = 10 * time.Second
		server.WriteTimeout = 30 * time.Second
	}

	errs := make(chan error, len(servers))
	for i, server := range servers {
		go func() { errs <- server.Serve(listeners[i]) }()
	}
	var err error
	select {
	case <-ctx.Done():
	case err = <-errs:
	}
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, server := range servers {
		if shutdownErr := server.Shutdown(shutdown); shutdownErr != nil {
			err = errors.Join(err, shutdownErr, server.Close())
		}
	}
	return err
}

// decodeBody parses a JSON request body into value, rejecting another media
// type, unknown fields and trailing data.
func decodeBody(r *http.Request, body []byte, value any) *result {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return &result{status: http.StatusUnsupportedMediaType, body: errorBody("unsupported_media_type")}
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return &result{status: http.StatusBadRequest, body: errorBody("invalid_request")}
	}
	if _, err := decoder.Token(); err != io.EOF {
		return &result{status: http.StatusBadRequest, body: errorBody("invalid_request")}
	}
	return nil
}

func errorBody(code string) map[string]string { return map[string]string{"error": code} }

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}
