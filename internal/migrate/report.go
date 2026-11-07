// The migration report: the import report with the ledger's counts, and the exit status it implies.

package migrate

import (
	"fmt"

	"github.com/Alex-Zechariah-02/apiward/internal/exchange"
	"github.com/Alex-Zechariah-02/apiward/internal/openapi"
)

// Report is the import report extended with the ledger's account. Every
// list is present, never null.
type Report struct {
	*openapi.Report
	LedgerSummary      Summary            `json:"ledger_summary"`
	StaleDecisions     []string           `json:"stale_decisions"`
	CredentialsSkipped []exchange.Skipped `json:"credentials_skipped"`
	CarriedCount       int                `json:"carried_count"`
}

// Summary counts ledger items per class, and approximations per direction.
type Summary struct {
	Exact          int        `json:"exact"`
	Equivalent     int        `json:"equivalent"`
	Approximated   Directions `json:"approximated"`
	NotExpressible int        `json:"not_expressible"`
}

// Directions counts approximations by how they differ.
type Directions struct {
	Stricter int `json:"stricter"`
	Looser   int `json:"looser"`
	Both     int `json:"both"`
}

func (s Summary) String() string {
	return fmt.Sprintf("%d exact, %d equivalent, %d approximated, %d not expressible", s.Exact, s.Equivalent,
		s.Approximated.Stricter+s.Approximated.Looser+s.Approximated.Both, s.NotExpressible)
}

func newReport(imported *openapi.Report, ledger *exchange.Ledger) *Report {
	r := &Report{Report: imported, StaleDecisions: []string{}, CredentialsSkipped: ledger.CredentialsSkipped,
		CarriedCount: len(ledger.Carried)}
	counts := map[exchange.Class]*int{exchange.Exact: &r.LedgerSummary.Exact, exchange.Equivalent: &r.LedgerSummary.Equivalent,
		exchange.NotExpressible: &r.LedgerSummary.NotExpressible}
	directions := map[exchange.Direction]*int{exchange.Stricter: &r.LedgerSummary.Approximated.Stricter,
		exchange.Looser: &r.LedgerSummary.Approximated.Looser, exchange.Both: &r.LedgerSummary.Approximated.Both}
	for _, item := range ledger.Items {
		if item.Class == exchange.Approximated {
			*directions[item.Direction]++
		} else {
			*counts[item.Class]++
		}
	}
	return r
}

// Exit is complete only when every setting is exact or equivalent and
// nothing was changed, refused, widened, carried, skipped or stale.
func (r *Report) Exit() int {
	s := r.LedgerSummary
	approximated := s.Approximated.Stricter + s.Approximated.Looser + s.Approximated.Both
	if r.Report.Exit() != openapi.ExitComplete || len(r.Changed) > 0 || approximated > 0 || s.NotExpressible > 0 ||
		len(r.StaleDecisions) > 0 || r.CarriedCount > 0 || len(r.CredentialsSkipped) > 0 {
		return openapi.ExitPartial
	}
	return openapi.ExitComplete
}
