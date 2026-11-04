// Test fixture request routing, matching and replies.

package testfixture

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"
)

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
