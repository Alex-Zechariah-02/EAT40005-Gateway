// Route hosts and headers: the conditions they become, and back.

package kong

import (
	"strings"

	"github.com/Alex-Zechariah-02/apiward/internal/exchange"
	"github.com/Alex-Zechariah-02/apiward/internal/policy"
	"github.com/Alex-Zechariah-02/apiward/internal/semantics"
)

// routeWhen reads a route's hosts and headers as the operation member
// x-apiward-when, or returns nil when it has neither. It returns a refusal
// for what conditions cannot state: a host with a port or a trailing
// wildcard, and a header with several values or a regular expression. The
// compiler checks the names and values themselves.
func routeWhen(route map[string]any) (map[string]any, string) {
	hosts, headers := texts(route["hosts"]), object(route["headers"])
	if len(hosts) == 0 && len(headers) == 0 {
		if present(route["hosts"]) || present(route["headers"]) {
			return nil, CodeMatch + ": hosts must be a list of names and headers an object of value lists."
		}
		return nil, ""
	}
	when := map[string]any{}
	if len(hosts) > 0 {
		var lower []string
		for _, host := range hosts {
			host = strings.ToLower(host)
			wildcards := strings.Count(host, "*")
			if strings.Contains(host, ":") || wildcards > 1 || wildcards == 1 && !strings.HasPrefix(host, "*.") {
				return nil, CodeMatch + ": the host " + policy.Clip(host) + " has a port or a trailing wildcard, which conditions cannot state."
			}
			lower = append(lower, host)
		}
		when["hosts"] = toAny(lower)
	}
	var conditions []any
	for _, name := range sortedKeys(headers) {
		values := texts(headers[name])
		if len(values) != 1 || strings.HasPrefix(values[0], "~*") {
			return nil, CodeMatch + ": a header condition with one value can be expressed; " + policy.Clip(name) + " has several or a regular expression."
		}
		conditions = append(conditions, map[string]any{"name": name, "value": values[0]})
	}
	if len(conditions) > 0 {
		when["headers"] = conditions
	}
	return when, ""
}

// whenDistance compares Kong's matching with the gateway's: both select the
// route with the most conditions; Kong compares header values without
// regard to case, the gateway exactly, which only matters for letters.
func whenDistance(when map[string]any) semantics.Distance {
	for _, h := range list(when["headers"]) {
		value := text(object(h)["value"])
		if strings.ToLower(value) != strings.ToUpper(value) {
			return semantics.Distance{Class: exchange.Approximated, Direction: exchange.Both,
				Note: "Kong compares header values without regard to case; the gateway compares them exactly, so " +
					policy.Clip(value) + " in another case selects the operation without conditions."}
		}
	}
	return semantics.Distance{Class: exchange.Equivalent,
		Note: "Selected by host and header conditions; like Kong, the gateway prefers the route with the most conditions."}
}

// putWhen writes an operation's conditions as the route's hosts and
// headers, so a conditioned operation never becomes an unconditioned route.
func putWhen(route, when map[string]any) {
	if hosts := list(when["hosts"]); len(hosts) > 0 {
		route["hosts"] = hosts
	}
	headers := map[string]any{}
	for _, h := range list(when["headers"]) {
		headers[text(object(h)["name"])] = []any{text(object(h)["value"])}
	}
	if len(headers) > 0 {
		route["headers"] = headers
	}
}
