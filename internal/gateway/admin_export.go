// Security log CSV export.

package gateway

import (
	"encoding/csv"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var securityLogColumns = []string{"time", "request_id", "decision", "status", "code", "stage", "method", "path", "operation", "api", "subject", "roles", "client_address", "generation", "duration_ms"}

// writeSecurityLogCSV writes the rows as CSV, with coverage in headers. A
// value a spreadsheet would read as a formula is prefixed with an apostrophe:
// paths and account IDs come from clients.
func writeSecurityLogCSV(w http.ResponseWriter, result securityLogResult, next *string, missing []string) {
	header := w.Header()
	header.Set("Content-Type", "text/csv; charset=utf-8")
	header.Set("Content-Disposition", `attachment; filename="security-log.csv"`)
	header.Set("X-Security-Log-Complete", strconv.FormatBool(next == nil))
	header.Set("X-Security-Log-Files-Searched", strings.Join(result.searched, ","))
	header.Set("X-Security-Log-Files-Missing", strings.Join(missing, ","))
	header.Set("X-Security-Log-Unreadable-Lines", strconv.Itoa(result.unreadable))
	if next != nil {
		header.Set("X-Security-Log-Next", *next)
	}
	w.WriteHeader(http.StatusOK)
	out := csv.NewWriter(w)
	out.Write(securityLogColumns)
	text := func(value *string) string {
		if value == nil {
			return ""
		}
		return *value
	}
	for _, v := range result.items {
		out.Write([]string{
			v.Time.UTC().Format(time.RFC3339Nano), v.RequestID, v.Decision, strconv.Itoa(v.Status), text(v.Code), v.Stage,
			v.Method, safeCell(v.Path), text(v.Operation), text(v.API), safeCell(text(v.Subject)), safeCell(strings.Join(v.Roles, " ")),
			v.ClientAddress, v.Generation, strconv.FormatFloat(v.DurationMS, 'f', -1, 64),
		})
	}
	out.Flush()
}

// safeCell keeps a spreadsheet from running a client-chosen value as a formula.
func safeCell(value string) string {
	if value != "" && strings.ContainsRune("=+-@\t\r", rune(value[0])) {
		return "'" + value
	}
	return value
}
