// Browser access on the data plane: CORS preflights answered by the gateway,
// and the API's CORS and security headers on every answer once its
// operation is known.

package gateway

import (
	"net/http"
	"strings"

	"github.com/Alex-Zechariah-02/apiward/internal/audit"
	"github.com/Alex-Zechariah-02/apiward/internal/compile"
)

// keyPreflight is the counter key type of CORS preflights; their counters
// are per API and client address, apart from any operation's.
const keyPreflight = "preflight"

// isPreflight reports whether r is a CORS preflight request.
func isPreflight(r *http.Request) bool {
	return r.Method == http.MethodOptions && r.Header.Get("Origin") != "" && r.Header.Get("Access-Control-Request-Method") != ""
}

// preflight answers a CORS preflight on a path that belongs to an API with
// browser access, and reports whether it did. Preflights are never
// forwarded: the answer is 204 with the API's CORS headers when the origin,
// method and requested headers are allowed, and 204 without them otherwise.
// The API's address allow list and its preflight rate rule apply first.
func (h *Handler) preflight(q *request, g *compile.Generation, path string, in compile.Inbound) bool {
	header := in.Header
	rule, handled := g.Preflight(path, header.Get("Access-Control-Request-Method"), in)
	if !handled {
		return false
	}
	if rule != nil {
		q.event.Operation, q.event.API, q.event.Upstream = rule.Operation(), rule.API(), rule.Upstream()
		if !rule.AddressAllowed(q.event.ClientAddress) {
			q.reject(audit.StageRoute, http.StatusForbidden, "address_not_allowed")
			return true
		}
		if limit := rule.PreflightRate(); limit != nil {
			key := counterKey{operation: rule.API(), keyType: keyPreflight, key: q.event.ClientAddress}
			if !h.admitRate(q, audit.StageRoute, key, limit) {
				return true
			}
		}
	}
	var allowed http.Header
	if rule != nil && rule.CORS() != nil {
		allowed = rule.CORS().Preflight(header.Get("Origin"), header.Get("Access-Control-Request-Method"), header.Values("Access-Control-Request-Headers"))
	}
	for name, values := range allowed {
		q.w.Header()[name] = values
	}
	q.answerPreflight(allowed != nil)
	return true
}

// apiWriter applies the API's response rules when the final status is
// written: upstream CORS headers are replaced by the gateway's, listed
// headers removed and missing security headers added.
type apiWriter struct {
	http.ResponseWriter
	cors     *compile.CORS
	response *compile.ResponseRule
	origin   string
	tls      bool
	applied  bool
}

// newAPIWriter wraps w with rule's API response rules; w is returned
// unchanged when the API has none.
func newAPIWriter(w http.ResponseWriter, rule *compile.Rule, r *http.Request) http.ResponseWriter {
	if rule.CORS() == nil && rule.Response() == nil {
		return w
	}
	return &apiWriter{ResponseWriter: w, cors: rule.CORS(), response: rule.Response(), origin: r.Header.Get("Origin"), tls: r.TLS != nil}
}

func (a *apiWriter) WriteHeader(status int) {
	// Informational (1xx) responses precede the final one.
	if !a.applied && status >= 200 {
		a.applied = true
		a.apply()
	}
	a.ResponseWriter.WriteHeader(status)
}

func (a *apiWriter) Write(data []byte) (int, error) {
	if !a.applied {
		a.WriteHeader(http.StatusOK)
	}
	return a.ResponseWriter.Write(data)
}

func (a *apiWriter) apply() {
	h := a.Header()
	if a.cors != nil {
		for name := range h {
			if strings.HasPrefix(name, "Access-Control-") {
				delete(h, name)
			}
		}
		for name, values := range a.cors.Response(a.origin) {
			if name == "Vary" {
				h.Add(name, values[0])
				continue
			}
			h[name] = values
		}
	}
	if a.response != nil {
		a.response.Apply(h, a.tls)
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (a *apiWriter) Unwrap() http.ResponseWriter { return a.ResponseWriter }
