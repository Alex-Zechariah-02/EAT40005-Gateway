package gateway

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	// securityLogWindow bounds the bytes read from disk at once; a page reads
	// at most securityLogPageScan bytes and an export securityLogExportScan,
	// so every answer ends in bounded time and memory however large the files.
	securityLogWindow     = 4 << 20
	securityLogPageScan   = 16 << 20
	securityLogExportScan = 256 << 20
	securityLogExportRows = 10000
)

// securityLogQuery holds the filters of one search. Empty filters match all.
type securityLogQuery struct {
	ref, subject, operation, decision string
	from, to                          time.Time
}

// auditRequestRecord is the part of a request's audit record the search
// reads; it is projected through requestView like the live views.
type auditRequestRecord struct {
	ID            string    `json:"event_id"`
	Kind          string    `json:"kind"`
	Time          time.Time `json:"time"`
	RequestID     string    `json:"request_id"`
	Stage         string    `json:"stage"`
	Decision      string    `json:"decision"`
	Status        int       `json:"status"`
	Error         string    `json:"error"`
	Method        string    `json:"method"`
	Path          string    `json:"path"`
	Operation     string    `json:"operation"`
	API           string    `json:"api"`
	Subject       string    `json:"subject"`
	Roles         []string  `json:"roles"`
	ClientAddress string    `json:"client_address"`
	Generation    string    `json:"generation"`
	DurationMS    float64   `json:"duration_ms"`
}

// securityLogCursor is where a search stopped: lines before offset in the
// file whose first record is first are still to be read. first identifies
// the file across a rotation, which renames the active file.
type securityLogCursor struct {
	File   string `json:"f"`
	First  string `json:"i"`
	Offset int64  `json:"o"`
}

type securityLogResult struct {
	items      []requestEvent
	next       *securityLogCursor
	searched   []string
	unreadable int
}

// handleSecurityLog searches the request records in the active and rotated
// audit files, newest first, with the same secret-free fields as the live
// views. format=csv or format=json exports up to securityLogExportRows rows.
func (c *Controller) handleSecurityLog(w http.ResponseWriter, r *http.Request, who caller) {
	values := r.URL.Query()
	query, field := parseSecurityLogQuery(values)
	if field != "" {
		adminError(w, http.StatusBadRequest, "bad_request", field)
		return
	}
	format := values.Get("format")
	if format != "" && format != "csv" && format != "json" {
		adminError(w, http.StatusBadRequest, "bad_request", "format")
		return
	}
	limit, scan := securityLogExportRows, int64(securityLogExportScan)
	if format == "" {
		limit, scan = 50, securityLogPageScan
		if text := values.Get("limit"); text != "" {
			n, err := strconv.Atoi(text)
			if err != nil || n < 1 || n > 100 {
				adminError(w, http.StatusBadRequest, "bad_request", "limit")
				return
			}
			limit = n
		}
	}
	var start *securityLogCursor
	if text := values.Get("after"); text != "" {
		start = decodeSecurityLogCursor(text)
		if start == nil {
			adminError(w, http.StatusBadRequest, "bad_request", "after")
			return
		}
	}
	files, missing, err := securityLogFiles(c.audit.path)
	if err != nil {
		adminError(w, http.StatusServiceUnavailable, "gateway_unavailable", "")
		return
	}
	result, err := searchSecurityLog(filepath.Dir(c.audit.path), files, start, query, limit, scan)
	if errors.Is(err, errCursorGone) {
		adminError(w, http.StatusGone, "cursor_gone", "after")
		return
	}
	if err != nil {
		adminError(w, http.StatusServiceUnavailable, "gateway_unavailable", "")
		return
	}
	var next *string
	if result.next != nil {
		encoded := encodeSecurityLogCursor(*result.next)
		next = &encoded
	}
	if format == "csv" {
		writeSecurityLogCSV(w, result, next, missing)
		return
	}
	if format == "json" {
		w.Header().Set("Content-Disposition", `attachment; filename="security-log.json"`)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":            result.items,
		"next":             next,
		"complete":         next == nil,
		"files_searched":   result.searched,
		"files_missing":    missing,
		"unreadable_lines": result.unreadable,
	})
}

func parseSecurityLogQuery(values map[string][]string) (securityLogQuery, string) {
	get := func(name string) string {
		if v := values[name]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	q := securityLogQuery{ref: strings.ToLower(get("ref")), subject: get("subject"), operation: get("operation"), decision: get("decision")}
	for name, value := range map[string]string{"ref": q.ref, "subject": q.subject, "operation": q.operation} {
		if len(value) > maxIDLength {
			return q, name
		}
	}
	if q.decision != "" && q.decision != "allowed" && q.decision != "refused" && q.decision != "failed" {
		return q, "decision"
	}
	for name, at := range map[string]*time.Time{"from": &q.from, "to": &q.to} {
		if text := get(name); text != "" {
			parsed, err := time.Parse(time.RFC3339Nano, text)
			if err != nil {
				return q, name
			}
			*at = parsed
		}
	}
	if !q.from.IsZero() && !q.to.IsZero() && q.to.Before(q.from) {
		return q, "to"
	}
	return q, ""
}

func (q securityLogQuery) matches(view requestEvent) bool {
	text := func(value *string) string {
		if value == nil {
			return ""
		}
		return *value
	}
	return (q.ref == "" || view.RequestID == q.ref) &&
		(q.subject == "" || text(view.Subject) == q.subject) &&
		(q.operation == "" || text(view.Operation) == q.operation) &&
		(q.decision == "" || view.Decision == q.decision) &&
		(q.from.IsZero() || !view.Time.Before(q.from)) &&
		(q.to.IsZero() || view.Time.Before(q.to))
}

// securityLogFiles lists the audit files newest first: the active file, then
// the rotated files from the highest number down. missing names the numbered
// files below the highest that are no longer present (moved away to archive).
func securityLogFiles(active string) (files, missing []string, err error) {
	entries, err := os.ReadDir(filepath.Dir(active))
	if err != nil {
		return nil, nil, err
	}
	prefix := filepath.Base(active) + "."
	numbers := []int{}
	for _, entry := range entries {
		suffix, found := strings.CutPrefix(entry.Name(), prefix)
		if n, err := strconv.Atoi(suffix); found && err == nil && n >= 1 && strconv.Itoa(n) == suffix {
			numbers = append(numbers, n)
		}
	}
	slices.Sort(numbers)
	files = []string{filepath.Base(active)}
	for i := len(numbers) - 1; i >= 0; i-- {
		files = append(files, prefix+strconv.Itoa(numbers[i]))
	}
	missing = []string{}
	if len(numbers) > 0 {
		for n := 1; n < numbers[len(numbers)-1]; n++ {
			if !slices.Contains(numbers, n) {
				missing = append(missing, prefix+strconv.Itoa(n))
			}
		}
	}
	return files, missing, nil
}
