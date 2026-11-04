// Hop-by-hop and untrusted header removal.

package gateway

import (
	"net/http"
	"strings"
)

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
