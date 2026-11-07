// Distance: how closely the gateway's own primitive matches a source primitive.

package semantics

import "github.com/Alex-Zechariah-02/apiward/internal/exchange"

// Distance is a ledger class with its direction and a note for the report.
type Distance struct {
	Class     exchange.Class
	Direction exchange.Direction
	Note      string
}

func exact() Distance { return Distance{Class: exchange.Exact} }

func equivalent(note string) Distance { return Distance{Class: exchange.Equivalent, Note: note} }

func approximated(direction exchange.Direction, note string) Distance {
	return Distance{Class: exchange.Approximated, Direction: direction, Note: note}
}

func inexpressible(note string) Distance { return Distance{Class: exchange.NotExpressible, Note: note} }
