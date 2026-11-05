// Admin API request decoding, preconditions, paging and JSON replies.

package gateway

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/Alex-Zechariah-02/apiward/internal/policy"
	"github.com/Alex-Zechariah-02/apiward/internal/store"
)

// decodeBody reads a bounded JSON request body into value. It refuses another
// media type, an oversized body, repeated or non-lowercase members, unknown
// fields and trailing data, and answers the request itself when it does.
func decodeBody(w http.ResponseWriter, r *http.Request, limit int64, value any) bool {
	if !isJSONMediaType(r.Header.Values("Content-Type")) {
		adminError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "")
		return false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
		adminError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "")
		return false
	}
	if err != nil || policy.CheckMembers(body, policy.MaxPolicyDepth+2) != nil || policy.DecodeStrict(body, value) != nil {
		adminError(w, http.StatusBadRequest, "bad_request", "")
		return false
	}
	return true
}

// ifMatch reads the required policy precondition "r<N>". It answers 428 when
// it is missing and 400 when it is malformed.
func ifMatch(w http.ResponseWriter, r *http.Request) (sql.NullInt64, bool) {
	values := r.Header.Values("If-Match")
	if len(values) == 0 {
		adminError(w, http.StatusPreconditionRequired, "precondition_required", "")
		return sql.NullInt64{}, false
	}
	text, ok := strings.CutPrefix(values[0], `"r`)
	text, closed := strings.CutSuffix(text, `"`)
	revision, err := strconv.ParseInt(text, 10, 64)
	if len(values) != 1 || !ok || !closed || err != nil || revision < 0 || strconv.FormatInt(revision, 10) != text {
		adminError(w, http.StatusBadRequest, "bad_request", "if_match")
		return sql.NullInt64{}, false
	}
	return sql.NullInt64{Int64: revision, Valid: true}, true
}

func policyETag(revision int64) string { return fmt.Sprintf(`"r%d"`, revision) }

// idempotencyKey reads the required Idempotency-Key header.
func idempotencyKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	values := r.Header.Values("Idempotency-Key")
	valid := len(values) == 1 && len(values[0]) >= minKeyLength && len(values[0]) <= maxKeyLength
	for i := 0; valid && i < len(values[0]); i++ {
		ch := values[0][i]
		valid = (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '-' || ch == '_'
	}
	if !valid {
		adminError(w, http.StatusBadRequest, "bad_request", "idempotency_key")
		return "", false
	}
	return values[0], true
}

// adminTransition names a mutation by its administrator and idempotency key,
// so two administrators can never collide and a retry finds its own record.
func adminTransition(who caller, key, kind, target string, expected sql.NullInt64, payload any) store.TransitionRecord {
	return store.TransitionRecord{
		ID:       "admin:" + who.admin.Name + ":" + key,
		Kind:     kind,
		Actor:    who.admin.Name,
		Target:   target,
		Expected: expected,
		Digest:   requestDigest(kind, target, expected, payload),
	}
}

// page reads the list parameters: limit (1 to 100) and an optional cursor.
// Any other parameter is refused, except those named in extra, which the
// caller reads and checks; every parameter may appear only once. A query that
// does not parse is refused too: URL.Query would drop its malformed pairs,
// and the listing would then answer for parameters the client never meant.
func page(w http.ResponseWriter, r *http.Request, extra ...string) (string, int, bool) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	limit := defaultPageSize
	valid := err == nil && knownQuery(query, append([]string{"limit", "after"}, extra...)...)
	if text := query.Get("limit"); valid && text != "" {
		n, err := strconv.Atoi(text)
		valid = err == nil && n >= 1 && n <= maxPageSize && strconv.Itoa(n) == text
		limit = n
	}
	if !valid {
		adminError(w, http.StatusBadRequest, "bad_request", "page")
		return "", 0, false
	}
	return query.Get("after"), limit, true
}

// numericCursor reads an integer list cursor; absent means from the start.
func numericCursor(w http.ResponseWriter, after string) (int64, bool) {
	if after == "" {
		return 0, true
	}
	n, err := strconv.ParseInt(after, 10, 64)
	if err != nil || n < 0 || strconv.FormatInt(n, 10) != after {
		adminError(w, http.StatusBadRequest, "bad_request", "page")
		return 0, false
	}
	return n, true
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	text := r.PathValue("id")
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil || n < 1 || strconv.FormatInt(n, 10) != text {
		adminError(w, http.StatusNotFound, "not_found", "")
		return 0, false
	}
	return n, true
}

type list struct {
	Items any    `json:"items"`
	Next  string `json:"next,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}
