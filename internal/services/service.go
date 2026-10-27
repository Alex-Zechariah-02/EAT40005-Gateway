package services

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
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	josejson "github.com/go-jose/go-jose/v4/json"
)

type Config struct {
	Name                string
	Secret              []byte
	InventoryURL        string
	OrdersURL           string
	SigningSettingsFile string
}

type result struct {
	status int
	body   any
	effect string
}
type apiError struct {
	Error   string `json:"error"`
	Message string `json:"message"`
	Field   string `json:"field,omitempty"`
}

func failure(status int, code, message, field string) result {
	return result{status: status, body: apiError{code, message, field}}
}
func notFound() result { return failure(404, "not_found", "That resource was not found.", "") }

type resource interface {
	handle(*http.Request, []byte) result
	reset()
	snapshot() any
}

type Receipt struct {
	Seq           int       `json:"seq"`
	Time          time.Time `json:"time"`
	RequestID     string    `json:"request_id"`
	Generation    string    `json:"generation"`
	Method        string    `json:"method"`
	Target        string    `json:"target"`
	Status        int       `json:"status"`
	Effect        string    `json:"effect,omitempty"`
	Authorization bool      `json:"authorization"`
	Subject       string    `json:"subject,omitempty"`
	Roles         string    `json:"roles,omitempty"`
	BodyBytes     int       `json:"body_bytes"`
}

type Service struct {
	mu             sync.Mutex
	name, instance string
	secret         []byte
	data           resource
	receipts       []Receipt
}

func New(cfg Config) (*Service, error) {
	if len(cfg.Secret) < 32 || len(cfg.Secret) > 4096 {
		return nil, errors.New("service secret must contain 32 to 4096 bytes")
	}
	s := &Service{name: cfg.Name, secret: slices.Clone(cfg.Secret), instance: randomID()}
	switch cfg.Name {
	case "inventory":
		s.data = &inventory{}
	case "orders":
		client, err := newServiceClient(cfg.InventoryURL, cfg.Secret)
		if err != nil {
			return nil, err
		}
		s.data = &orders{inventory: client}
	case "support":
		s.data = &support{}
	case "reports":
		client, err := newServiceClient(cfg.OrdersURL, cfg.Secret)
		if err != nil {
			return nil, err
		}
		s.data = &reports{orders: client}
	case "accounts":
		accounts, err := newAccounts(cfg.SigningSettingsFile)
		if err != nil {
			return nil, err
		}
		s.data = accounts
	default:
		return nil, fmt.Errorf("unknown service %q", cfg.Name)
	}
	s.data.reset()
	return s, nil
}

func randomID() string {
	var value [16]byte
	rand.Read(value[:])
	return hex.EncodeToString(value[:])
}

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Service", s.name)
	keyRequired := strings.HasPrefix(r.URL.Path, "/internal/") && !s.serviceKey(r)
	var body []byte
	var err error
	if keyRequired {
		if r.ContentLength != 0 || len(r.TransferEncoding) > 0 {
			w.Header().Set("Connection", "close")
		}
	} else {
		body, err = io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res := notFound()
	var tooLarge *http.MaxBytesError
	switch {
	case keyRequired:
		res = failure(401, "service_key_required", "A service key is required.", "")
	case len(s.receipts) >= 10000:
		res = failure(503, "receipts_full", "The request record is full. Reset this service before continuing.", "")
	case errors.As(err, &tooLarge):
		res = failure(413, "payload_too_large", "The request body is too large.", "")
	case err != nil:
		res = failure(400, "invalid_request", "The request body could not be read.", "")
	case len(body) > 0 && r.Method != "POST" && r.Method != "PATCH" && r.Method != "PUT":
		res = failure(400, "invalid_request", "This operation accepts no request body.", "")
	case r.Method != "GET" && r.Method != "HEAD" && r.URL.RawQuery != "":
		res = invalidQuery()
	default:
		res = s.data.handle(r, body)
	}
	if len(s.receipts) < 10000 {
		s.receipts = append(s.receipts, Receipt{Seq: len(s.receipts) + 1, Time: time.Now().UTC(), RequestID: bounded(r.Header.Get("X-Request-Id"), 128), Generation: bounded(r.Header.Get("X-Gateway-Generation"), 256), Method: r.Method, Target: bounded(r.URL.EscapedPath(), 512), Status: res.status, Effect: res.effect, Authorization: r.Header.Get("Authorization") != "", Subject: bounded(r.Header.Get("X-Gateway-Subject"), 128), Roles: bounded(r.Header.Get("X-Gateway-Roles"), 4096), BodyBytes: len(body)})
	}
	writeJSON(w, res)
}

func bounded(value string, n int) string {
	if len(value) > n {
		return value[:n]
	}
	return value
}
func (s *Service) serviceKey(r *http.Request) bool {
	values := r.Header.Values("X-Service-Key")
	return len(values) == 1 && subtle.ConstantTimeCompare([]byte(values[0]), s.secret) == 1
}
func writeJSON(w http.ResponseWriter, res result) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(res.status)
	json.NewEncoder(w).Encode(res.body)
}

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
	return State{s.name, "fetch-1", s.instance, hex.EncodeToString(sum[:]), data}
}

func (s *Service) Control(secret []byte) http.Handler {
	secret = slices.Clone(secret)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Service", s.name)
		values := r.Header.Values("Authorization")
		if len(secret) < 32 || len(secret) > 4096 || len(values) != 1 || subtle.ConstantTimeCompare([]byte(values[0]), append([]byte("Bearer "), secret...)) != 1 {
			writeJSON(w, failure(401, "unauthenticated", "Control authentication is required.", ""))
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		switch {
		case r.Method == "GET" && r.URL.Path == "/receipts":
			writeJSON(w, result{status: 200, body: map[string]any{"service": s.name, "instance": s.instance, "receipts": append([]Receipt{}, s.receipts...)}})
		case r.Method == "GET" && r.URL.Path == "/state":
			writeJSON(w, result{status: 200, body: s.state()})
		case r.Method == "POST" && r.URL.Path == "/reset":
			s.data.reset()
			s.receipts = nil
			writeJSON(w, result{status: 200, body: s.state()})
		default:
			writeJSON(w, notFound())
		}
	})
}

func Run(ctx context.Context, s *Service, listen, control string, secret []byte) error {
	if closer, ok := s.data.(interface{ close() }); ok {
		defer closer.close()
	}
	if len(secret) < 32 || len(secret) > 4096 {
		return errors.New("control secret must contain 32 to 4096 bytes")
	}
	servers := []*http.Server{{Addr: listen, Handler: s}, {Addr: control, Handler: s.Control(secret)}}
	listeners := []net.Listener{}
	defer func() {
		for _, l := range listeners {
			l.Close()
		}
	}()
	for _, server := range servers {
		address, err := netip.ParseAddrPort(server.Addr)
		if err != nil || !address.Addr().IsLoopback() {
			return errors.New("service listeners must be literal loopback addresses")
		}
		listener, err := net.Listen("tcp", server.Addr)
		if err != nil {
			return err
		}
		listeners = append(listeners, listener)
		server.ReadHeaderTimeout = 5 * time.Second
		server.ReadTimeout = 10 * time.Second
		server.WriteTimeout = 10 * time.Second
	}
	errs := make(chan error, 2)
	for i, server := range servers {
		go func() { errs <- server.Serve(listeners[i]) }()
	}
	var result error
	select {
	case <-ctx.Done():
	case result = <-errs:
	}
	stop, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, server := range servers {
		if err := server.Shutdown(stop); err != nil {
			result = errors.Join(result, err, server.Close())
		}
	}
	if errors.Is(result, http.ErrServerClosed) {
		return nil
	}
	return result
}

func decodeBody(r *http.Request, body []byte, limit int, value any) *result {
	if len(body) > limit {
		v := failure(413, "payload_too_large", "The request body is too large.", "")
		return &v
	}
	values := r.Header.Values("Content-Type")
	media, parameters, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	validMedia := len(values) == 1 && err == nil && media == "application/json"
	for name, value := range parameters {
		if name != "charset" || !strings.EqualFold(value, "utf-8") {
			validMedia = false
		}
	}
	if !validMedia {
		v := failure(415, "unsupported_media_type", "Send a JSON request body.", "")
		return &v
	}
	// Reject duplicate names before decoding and permit only the lowercase
	// member names used by the service contracts, including nested objects.
	var document any
	if !utf8.Valid(body) || hasLoneSurrogate(body) || josejson.Unmarshal(body, &document) != nil || !validMembers(document, 0) {
		v := failure(400, "invalid_request", "The JSON request is invalid.", "")
		return &v
	}
	if _, ok := document.(map[string]any); !ok {
		v := failure(400, "invalid_request", "Send a JSON object.", "")
		return &v
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(value) != nil {
		v := failure(400, "invalid_request", "The JSON request is invalid.", "")
		return &v
	}
	if decoder.Decode(&document) != io.EOF {
		v := failure(400, "invalid_request", "Send exactly one JSON value.", "")
		return &v
	}
	return nil
}

func validMembers(value any, depth int) bool {
	if depth > 16 {
		return false
	}
	switch value := value.(type) {
	case nil:
		return false
	case map[string]any:
		for key, item := range value {
			if key == "" {
				return false
			}
			for _, c := range key {
				if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') {
					return false
				}
			}
			if !validMembers(item, depth+1) {
				return false
			}
		}
	case []any:
		for _, item := range value {
			if !validMembers(item, depth+1) {
				return false
			}
		}
	}
	return true
}

// Preserve the gateway's strict Unicode profile instead of letting JSON
// decoders silently replace an unpaired UTF-16 escape with U+FFFD.
func hasLoneSurrogate(data []byte) bool {
	surrogate := func(i int) (rune, bool) {
		if i+6 > len(data) || data[i] != '\\' || data[i+1] != 'u' {
			return 0, false
		}
		code, err := strconv.ParseUint(string(data[i+2:i+6]), 16, 16)
		return rune(code), err == nil && code >= 0xd800 && code <= 0xdfff
	}
	for i := 0; i < len(data); i++ {
		if data[i] != '\\' {
			continue
		}
		code, found := surrogate(i)
		switch {
		case !found:
			i++
		case code >= 0xdc00:
			return true
		default:
			low, ok := surrogate(i + 6)
			if !ok || low < 0xdc00 {
				return true
			}
			i += 11
		}
	}
	return false
}

func query(r *http.Request, names ...string) (url.Values, bool) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, false
	}
	for name, list := range values {
		if len(list) != 1 || !slices.Contains(names, name) {
			return nil, false
		}
	}
	return values, true
}
func pagination(q url.Values) (int, string, bool) {
	n := 20
	var err error
	if text := q.Get("limit"); text != "" {
		n, err = strconv.Atoi(text)
		if strconv.Itoa(n) != text {
			return 0, "", false
		}
	}
	return n, q.Get("after"), err == nil && n >= 1 && n <= 100
}
func invalidQuery() result { return failure(400, "invalid_query", "Check the query parameters.", "") }
func length(value string, min, max int) bool {
	return utf8.ValidString(value) && utf8.RuneCountInString(value) >= min && utf8.RuneCountInString(value) <= max
}
func hasRole(r *http.Request, role string) bool {
	return slices.Contains(strings.Split(r.Header.Get("X-Gateway-Roles"), ","), role)
}
func idempotencyKey(value string) bool {
	if len(value) < 16 || len(value) > 64 {
		return false
	}
	for _, c := range value {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}
