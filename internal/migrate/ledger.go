// The OpenAPI reader: one classified ledger item per setting of a description.

package migrate

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"

	"github.com/Alex-Zechariah-02/apiward/internal/exchange"
	"github.com/Alex-Zechariah-02/apiward/internal/openapi"
	"github.com/Alex-Zechariah-02/apiward/internal/semantics"
)

// openapiLedger classifies what the import does with a description: each
// operation, each change the gateway makes to one, each unchecked
// parameter, each scope and the server addresses. Documentation members
// (responses, links, callbacks, webhooks, tags) are not settings.
func openapiLedger(root map[string]any, report *openapi.Report, data []byte) *exchange.Ledger {
	sum := sha256.Sum256(data)
	version, _ := root["openapi"].(string)
	ledger := exchange.New(exchange.Source{Format: "openapi", FormatVersion: version, SHA256: hex.EncodeToString(sum[:])})
	add := func(id, facet, source, target string, d semantics.Distance) {
		ledger.Items = append(ledger.Items, exchange.Item{ID: "openapi:" + id, Facet: facet, Class: d.Class,
			Direction: d.Direction, Source: source, Target: target, Note: d.Note})
	}
	names := map[string]string{} // operation identifier → "METHOD path"
	for _, m := range report.Mapped {
		names[m.Operation] = m.Method + " " + m.Path
		at := pointer(m.Path, m.Method)
		add("operation:"+names[m.Operation], "route", at, at, mappedDistance(m))
	}
	for _, w := range report.Widened {
		names[w.Operation] = w.Method + " " + w.Path
		_, d := semantics.PathRule{Match: semantics.LastSegmentTemplate, Value: w.Path}.Convert()
		at := pointer(w.Path, w.Method)
		add("operation:"+names[w.Operation], "route", at, at, d)
	}
	for _, r := range report.Refused {
		d := semantics.Distance{Class: exchange.NotExpressible, Note: r.Code + ": " + r.Message}
		if r.Method == "" {
			add("path:"+r.Path, "route", "#/paths/"+escape(r.Path), "", d)
			continue
		}
		add("operation:"+r.Method+" "+r.Path, "route", pointer(r.Path, r.Method), "", d)
	}
	for _, c := range report.Changed {
		method, path, _ := strings.Cut(names[c.Operation], " ")
		at := pointer(path, method)
		add("operation:"+names[c.Operation]+":"+c.Field, "validation", at, at+"/requestBody",
			semantics.Distance{Class: exchange.Approximated, Direction: exchange.Stricter, Note: c.Note})
	}
	for _, n := range report.NotEnforced {
		method, path, _ := strings.Cut(names[n.Operation], " ")
		add("parameter:"+names[n.Operation]+":"+n.In+"."+n.Parameter, "validation", pointer(path, method), "",
			semantics.Distance{Class: exchange.NotExpressible, Note: "The gateway does not check " + n.In + " parameters."})
	}
	for _, scope := range report.ScopesNotMapped {
		add("scope:"+scope, "roles", "#/components/securitySchemes", "", semantics.Distance{Class: exchange.NotExpressible,
			Note: "Token scopes are not checked; the roles permitted to each operation decide who may call it."})
	}
	if slices.Contains(report.Ignored, "servers") {
		source := "#/paths"
		if _, atRoot := root["servers"]; atRoot {
			source = "#/servers"
		}
		add("member:servers", "upstream", source, "", semantics.Distance{Class: exchange.NotExpressible,
			Note: "Server addresses are not read; migrate policy binds the API to --upstream."})
	}
	slices.SortFunc(ledger.Items, func(a, b exchange.Item) int { return cmp.Compare(a.ID, b.ID) })
	return ledger
}

// mappedDistance is exact, except where the import requires sign-in that
// the description does not state, which is stricter.
func mappedDistance(m openapi.Mapped) semantics.Distance {
	if m.Note == openapi.NoteDefaultSignIn {
		return semantics.Distance{Class: exchange.Approximated, Direction: exchange.Stricter,
			Note: "The description states no security; the gateway requires sign-in."}
	}
	rule := semantics.PathRule{Match: semantics.Exact, Value: m.Target}
	if strings.HasSuffix(m.Target, "*") {
		rule.Match = semantics.DeeperOnly // the description states x-apiward-target
	}
	_, d := rule.Convert()
	d.Note = m.Note
	return d
}

// pointer is the JSON pointer of an operation in a description.
func pointer(path, method string) string {
	return "#/paths/" + escape(path) + "/" + strings.ToLower(method)
}

// escape encodes one JSON pointer segment (RFC 6901).
func escape(segment string) string {
	return strings.NewReplacer("~", "~0", "/", "~1").Replace(segment)
}
