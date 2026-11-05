// Request body reading and validation against the captured rule.

package gateway

import (
	"bytes"
	"io"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

// rejection is a request refused at a body or validation stage.
type rejection struct {
	status int
	code   string
	reason string
}

// readBody enforces the operation's body declaration: no body for a bodyless
// operation; otherwise the size ceiling, a JSON media type, a complete read
// and a bounded, unambiguous JSON document. The request body is replaced with
// the bytes read, so nothing unchecked can stream upstream. It returns the
// decoded document.
//
// The size ceiling is the smaller of the operation's and the global one, and
// it bounds the bytes read as well as the declared length: a chunked body, or
// an HTTP/2 body without a content-length field, declares no length, so a
// check of the declared length alone would let it pass the global ceiling.
func readBody(r *http.Request, rule *bodyRule, globalMaxBytes int64) (any, *rejection) {
	if rule == nil {
		if r.ContentLength != 0 || len(r.TransferEncoding) > 0 {
			return nil, &rejection{http.StatusBadRequest, "bad_request", "unexpected_body"}
		}
		return nil, nil
	}
	maxBytes := min(rule.maxBytes, globalMaxBytes)
	if r.ContentLength > maxBytes {
		return nil, &rejection{http.StatusRequestEntityTooLarge, "payload_too_large", "too_large"}
	}
	if !isJSONMediaType(r.Header.Values("Content-Type")) {
		return nil, &rejection{http.StatusUnsupportedMediaType, "unsupported_media_type", "media_type"}
	}
	// The policy compiler bounds the operation's ceiling, so maxBytes+1
	// cannot overflow.
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBytes+1))
	if err != nil {
		return nil, &rejection{http.StatusBadRequest, "bad_request", "read_failed"}
	}
	if int64(len(body)) > maxBytes {
		return nil, &rejection{http.StatusRequestEntityTooLarge, "payload_too_large", "too_large"}
	}
	document, reason := policy.DecodeJSON(body)
	if reason != "" {
		return nil, &rejection{http.StatusBadRequest, "bad_request", reason}
	}

	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.TransferEncoding = nil
	r.Header.Set("Content-Length", strconv.Itoa(len(body)))
	return document, nil
}

// isJSONMediaType accepts exactly one Content-Type field naming
// application/json, optionally with the parameter charset=utf-8.
func isJSONMediaType(fields []string) bool {
	if len(fields) != 1 {
		return false
	}
	mediaType, params, err := mime.ParseMediaType(fields[0])
	if err != nil || mediaType != "application/json" {
		return false
	}
	for name, value := range params {
		if name != "charset" || !strings.EqualFold(value, "utf-8") {
			return false
		}
	}
	return true
}

// validateRequest checks the declared request headers and the decoded body
// against the operation's schemas. It returns the rejection reason, or "".
// Library error text can quote the input, so it is never returned.
func validateRequest(header http.Header, document any, r *rule) string {
	// The proxy removes every field named in Connection, so a validated field
	// named there would not reach the upstream as it was checked: a declared
	// header, or the media type of a validated body. Such a request is refused
	// rather than forwarded without it.
	for _, name := range connectionNames(header) {
		validatedHeader := r.headers != nil && slices.Contains(r.headers.names, name)
		validatedMediaType := r.body != nil && name == "Content-Type"
		if validatedHeader || validatedMediaType {
			return "connection_nominated"
		}
	}
	if r.headers != nil {
		declared := map[string]any{}
		for _, name := range r.headers.names {
			values := header.Values(name)
			if len(values) > 1 {
				return "repeated_header"
			}
			if len(values) == 1 {
				declared[name] = values[0]
			}
		}
		if r.headers.schema.Validate(declared) != nil {
			return "header_mismatch"
		}
	}
	if r.body != nil && r.body.schema.Validate(document) != nil {
		return "schema_mismatch"
	}
	return ""
}
