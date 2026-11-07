// The decK writer: routes unchanged since import are restored from the carried originals; the rest are written from their operations.

package kong

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/Alex-Zechariah-02/apiward/internal/exchange"
)

// Options choose what an export may write beyond the API configuration.
type Options struct {
	WithUpstream bool // service addresses
	WithTrust    bool // issuer consumers with their public keys
	WithBlocked  bool // blocked accounts (Kong has none: listed as a manual step)
}

// placeholder is the service host written without --with-upstream (RFC 2606).
const placeholder = "upstream.invalid"

// ExportReport says which routes were written as read and what changed.
type ExportReport struct {
	Format      string   `json:"format"`
	Restored    []string `json:"restored"`
	Generated   []string `json:"generated"`
	Changed     []Change `json:"changed"`
	NotCarried  []Change `json:"not_carried"`
	ManualSteps []string `json:"manual_steps"`
}

// Change is one difference an export makes, for one route.
type Change struct {
	Route string `json:"route"`
	Field string `json:"field"`
	Note  string `json:"note"`
}

// Exit is complete only when every route was written as read and nothing
// is left to the operator.
func (r *ExportReport) Exit() int {
	if len(r.Changed)+len(r.NotCarried)+len(r.ManualSteps) > 0 {
		return 2
	}
	return 0
}

// writer holds one export.
type writer struct {
	document map[string]any
	ledger   *exchange.Ledger
	options  Options
	report   *ExportReport
	claimed  map[string]bool // operation pointers written
	dropped  map[string]bool // names and ids of routes written from their operations
	root     map[string]any  // the decK file read at import
}

// Write turns an exchange document and its ledger into a decK file.
func Write(document map[string]any, ledger *exchange.Ledger, options Options) (map[string]any, *ExportReport, error) {
	w := &writer{document: document, ledger: ledger, options: options, claimed: map[string]bool{}, dropped: map[string]bool{},
		report: &ExportReport{Format: Format, Restored: []string{}, Generated: []string{}, Changed: []Change{},
			NotCarried: []Change{}, ManualSteps: []string{}}}
	out := map[string]any{"_format_version": Version}
	original, err := w.original()
	if err != nil {
		return nil, nil, err
	}
	w.root = original
	view, err := Read(original, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("the carried Kong settings cannot be read: %w", err)
	}
	for _, name := range sortedKeys(original) {
		if !slices.Contains([]string{"_format_version", "services", "routes", "plugins", "consumers"}, name) {
			out[name] = original[name]
		}
	}
	services := []any{}
	for i, value := range list(original["services"]) {
		s := w.address(object(value))
		s["routes"] = w.routes(list(s["routes"]), "#/services/"+strconv.Itoa(i)+"/routes/", view)
		services = append(services, s)
	}
	topRoutes := w.routes(list(original["routes"]), "#/routes/", view)
	if extra := w.unclaimed(); len(extra) > 0 {
		services = append(services, map[string]any{"name": "apiward", "host": placeholder, "routes": extra})
		w.report.ManualSteps = append(w.report.ManualSteps, "Set the address of the service apiward, which serves operations that came from no Kong route.")
	}
	out["services"] = services
	if len(topRoutes) > 0 {
		out["routes"] = topRoutes
	}
	plugins := slices.DeleteFunc(slices.Clone(list(original["plugins"])), func(p any) bool {
		ref := reference(object(p)["route"])
		return ref != "" && w.dropped[ref]
	})
	if len(plugins) > 0 {
		out["plugins"] = plugins
	}
	w.identity(out, list(original["consumers"]))
	slices.Sort(w.report.Restored)
	slices.Sort(w.report.Generated)
	return out, w.report, nil
}

// original rebuilds the decK file read at import from the carried values;
// an exchange file from another format has none.
func (w *writer) original() (map[string]any, error) {
	root := map[string]any{"_format_version": Version}
	if w.ledger.Source.Format != Format {
		return root, nil
	}
	for _, c := range w.ledger.Carried {
		// Numbers stay json.Number, as the import's gate reads them.
		decoder := json.NewDecoder(bytes.NewReader(c.Value))
		decoder.UseNumber()
		var value any
		if c.Vendor != Format {
			continue
		}
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("carried value %s: %w", c.Path, err)
		}
		place(root, strings.Split(strings.TrimPrefix(c.Path, "#/"), "/"), value)
	}
	root["plugins"] = slices.DeleteFunc(list(root["plugins"]), func(p any) bool { return p == nil })
	return root, nil
}

// place sets a value at a pointer's tokens, growing lists as needed.
// Carried values come parent first, so a parent always exists already.
func place(container map[string]any, tokens []string, value any) {
	name := tokens[0]
	if len(tokens) == 1 {
		container[name] = value
		return
	}
	index, _ := strconv.Atoi(tokens[1])
	items := list(container[name])
	for len(items) <= index {
		items = append(items, nil)
	}
	container[name] = items
	if len(tokens) == 2 {
		items[index] = value
		return
	}
	if object(items[index]) == nil {
		items[index] = map[string]any{}
	}
	place(object(items[index]), tokens[2:], value)
}

// address writes a service's address only with --with-upstream.
func (w *writer) address(s map[string]any) map[string]any {
	if w.options.WithUpstream {
		return s
	}
	out := without(s, "url", "host")
	out["host"] = placeholder
	if raw := text(s["url"]); raw != "" {
		scheme, rest, _ := strings.Cut(raw, "://")
		out["protocol"] = scheme
		if _, path, hasPath := strings.Cut(rest, "/"); hasPath {
			out["path"] = "/" + path
		}
	}
	w.report.ManualSteps = append(w.report.ManualSteps, "Set the address of the service "+text(s["name"])+" (--with-upstream writes it).")
	return out
}

// routes restores each route whose operations are unchanged since import
// and writes the others from their current operations.
func (w *writer) routes(routes []any, prefix string, view *Result) []any {
	out := []any{}
	for j, value := range routes {
		route := object(value)
		targets := operationsOf(view, prefix+strconv.Itoa(j))
		if w.unchanged(view, targets) {
			for _, t := range targets {
				w.claimed[t] = true
			}
			out = append(out, route)
			w.report.Restored = append(w.report.Restored, text(route["name"]))
			continue
		}
		for _, t := range targets {
			if op := at(w.document, t); op != nil && !w.claimed[t] {
				w.claimed[t] = true
				out = append(out, w.generate(t, op, route))
			}
		}
	}
	return out
}

// operationsOf lists the operation pointers the reader made from a route.
func operationsOf(view *Result, route string) []string {
	var targets []string
	for _, item := range view.Ledger.Items {
		kind, _, _ := strings.Cut(strings.TrimPrefix(item.ID, Format+":"), ":")
		if kind == "operation" && item.Source == route && item.Target != "" && !slices.Contains(targets, item.Target) {
			targets = append(targets, item.Target)
		}
	}
	return targets
}

// unchanged reports whether every operation the route became is still in
// the document exactly as the reader wrote it.
func (w *writer) unchanged(view *Result, targets []string) bool {
	return !slices.ContainsFunc(targets, func(t string) bool {
		current := at(w.document, t)
		return current == nil || !same(current, at(view.Document, t))
	})
}

// at returns the operation a pointer #/paths/<path>/<method> names.
func at(document map[string]any, pointer string) map[string]any {
	path, method, found := strings.Cut(strings.TrimPrefix(pointer, "#/paths/"), "/")
	if !found {
		return nil
	}
	path = strings.NewReplacer("~1", "/", "~0", "~").Replace(path)
	return object(object(object(document["paths"])[path])[method])
}

// same compares two values as JSON reads them back, so Go values and
// values decoded from a file compare equal. A value that cannot be encoded
// counts as changed, so its route is written from its operations and reported.
func same(a, b any) bool {
	left, errLeft := json.Marshal(a)
	right, errRight := json.Marshal(b)
	if errLeft != nil || errRight != nil {
		return false
	}
	var x, y any
	if json.Unmarshal(left, &x) != nil || json.Unmarshal(right, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}
