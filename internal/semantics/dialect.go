// Schema dialect and failure-mode primitives.

package semantics

import "github.com/Alex-Zechariah-02/apiward/internal/exchange"

// Dialect is the JSON Schema dialect a source writes request schemas in.
type Dialect string

const (
	Draft4    Dialect = "draft-4"
	Draft7    Dialect = "draft-7"
	Draft2019 Dialect = "2019-09"
	Draft2020 Dialect = "2020-12"
	OpenAPI30 Dialect = "openapi-3.0"
)

// Same is the decision rule: whether the keyword with this value means in
// the dialect what it means in 2020-12, the gateway's dialect. Array-valued
// items (tuples), boolean exclusive bounds and nullable do not.
func (d Dialect) Same(keyword string, value any) bool {
	if d == Draft2020 {
		return true
	}
	switch keyword {
	case "items":
		_, tuple := value.([]any)
		return !tuple
	case "exclusiveMinimum", "exclusiveMaximum":
		_, modifier := value.(bool)
		return !modifier
	case "nullable":
		return d != OpenAPI30
	}
	return true
}

// Convert is the dialect's distance: the gateway's keyword subset means the
// same in every listed dialect, and a keyword that differs is refused.
func (d Dialect) Convert() Distance {
	if d == Draft2020 {
		return exact()
	}
	return equivalent("The keywords the gateway checks mean the same in " + string(d) + "; one that differs is refused.")
}

// Failure is what a gateway does when a control's backing store fails.
type Failure string

const (
	FailClosed Failure = "fail_closed"
	FailOpen   Failure = "fail_open"
)

// Admits is the decision rule while the store is failing.
func (f Failure) Admits() bool { return f == FailOpen }

// Convert is the distance: the gateway always fails closed.
func (f Failure) Convert() Distance {
	if f == FailClosed {
		return exact()
	}
	return approximated(exchange.Stricter, "The gateway refuses requests it cannot check instead of admitting them.")
}
