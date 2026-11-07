// The decK inventory: what a file holds, counted, without writing any setting or credential value.

package kong

import (
	"cmp"
	"slices"
	"strings"

	"github.com/Alex-Zechariah-02/apiward/internal/exchange"
)

// Inventory counts what a decK file holds and what cannot be imported.
type Inventory struct {
	Format      string             `json:"format"`
	Services    int                `json:"services"`
	Routes      []RoutePlugins     `json:"routes"`
	Consumers   int                `json:"consumers"`
	Credentials []exchange.Skipped `json:"credentials"`
	Facets      map[string]int     `json:"facets"`
	Unsupported []string           `json:"unsupported"`
}

// RoutePlugins names the plugins configured on one route.
type RoutePlugins struct {
	Route   string   `json:"route"`
	Plugins []string `json:"plugins"`
}

// Take reads a decoded decK file and counts it. Unsupported lists each
// refused item's identifier and code, never a value from the file.
func Take(root map[string]any, data []byte) (*Inventory, error) {
	result, err := Read(root, data)
	if err != nil {
		return nil, err
	}
	inventory := &Inventory{Format: Format, Services: len(list(root["services"])), Routes: []RoutePlugins{},
		Consumers: len(list(root["consumers"])), Credentials: result.Ledger.CredentialsSkipped,
		Facets: map[string]int{}, Unsupported: []string{}}
	scoped := map[string][]string{}
	for _, p := range list(root["plugins"]) {
		if ref := reference(object(p)["route"]); ref != "" {
			scoped[ref] = append(scoped[ref], text(object(p)["name"]))
		}
	}
	routes := list(root["routes"])
	for _, s := range list(root["services"]) {
		routes = append(routes, list(object(s)["routes"])...)
	}
	for _, value := range routes {
		route := object(value)
		name := cmp.Or(text(route["name"]), text(route["id"]))
		entry := RoutePlugins{Route: name, Plugins: slices.Concat(scoped[text(route["name"])], scoped[text(route["id"])])}
		for _, p := range list(route["plugins"]) {
			entry.Plugins = append(entry.Plugins, text(object(p)["name"]))
		}
		slices.Sort(entry.Plugins)
		entry.Plugins = append([]string{}, entry.Plugins...)
		inventory.Routes = append(inventory.Routes, entry)
	}
	slices.SortFunc(inventory.Routes, func(a, b RoutePlugins) int { return strings.Compare(a.Route, b.Route) })
	for _, item := range result.Ledger.Items {
		inventory.Facets[item.Facet]++
		if code, _, coded := strings.Cut(item.Note, ":"); item.Class == exchange.NotExpressible && coded && isCode(code) {
			inventory.Unsupported = append(inventory.Unsupported, strings.TrimPrefix(item.ID, Format+":")+": "+code)
		}
	}
	return inventory, nil
}

// isCode reports whether a note starts with a refusal code such as UNKNOWN_FIELD.
func isCode(word string) bool {
	return word != "" && strings.Trim(word, "ABCDEFGHIJKLMNOPQRSTUVWXYZ_") == ""
}
