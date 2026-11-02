package gateway

import (
	"bytes"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

var errCursorGone = errors.New("the file the search stopped in is no longer present")

// searchSecurityLog reads the files in dir newest first from start, at most
// scan bytes, until limit records match. Only the final record of each
// request (request.rejected or request.forwarded) is read, as the live views
// do. A line still being written at the end of the active file is skipped.
func searchSecurityLog(dir string, files []string, start *securityLogCursor, query securityLogQuery, limit int, scan int64) (securityLogResult, error) {
	result := securityLogResult{items: []requestEvent{}, searched: []string{}}
	index, offset := 0, int64(-1) // -1 reads a file from its end
	if start != nil {
		index = -1
		// The named file first, then any other: a rotation renames the
		// active file, which keeps its first record.
		order := slices.Clone(files)
		if i := slices.Index(order, start.File); i > 0 {
			order[0], order[i] = order[i], order[0]
		}
		for _, name := range order {
			if first, err := firstRecordID(filepath.Join(dir, name)); err == nil && first == start.First {
				index, offset = slices.Index(files, name), start.Offset
				break
			}
		}
		if index < 0 {
			return result, errCursorGone
		}
	}
	budget := scan
	for ; index < len(files); index, offset = index+1, -1 {
		name := files[index]
		first, err := firstRecordID(filepath.Join(dir, name))
		if errors.Is(err, os.ErrNotExist) {
			continue // archived since it was listed
		}
		if err != nil {
			return result, err
		}
		if budget <= 0 {
			result.next = &securityLogCursor{File: name, First: first, Offset: offset}
			return result, nil
		}
		result.searched = append(result.searched, name)
		done, err := scanSecurityLogFile(filepath.Join(dir, name), name, first, offset, query, limit, &budget, &result)
		if err != nil || done {
			return result, err
		}
	}
	return result, nil
}

// scanSecurityLogFile searches one file backwards from end (-1: its current
// size). It reports done when the search stops inside the file, with
// result.next set where it stopped.
func scanSecurityLogFile(path, name, first string, end int64, query securityLogQuery, limit int, budget *int64, result *securityLogResult) (bool, error) {
	stop, stopped, err := scanLinesBackward(path, end, budget, func(line []byte, _ int64) bool {
		if view, ok := securityLogRecord(line, query, result); ok {
			result.items = append(result.items, view)
			return len(result.items) >= limit
		}
		return false
	})
	if err != nil {
		return false, err
	}
	if stopped {
		result.next = &securityLogCursor{File: name, First: first, Offset: stop}
	}
	return stopped, nil
}

// scanLinesBackward calls visit for each complete line of a file, newest
// first, from end (-1: its current size), reading at most *budget bytes in
// windows. visit returns true to stop. stop is where reading can resume:
// lines before it are unread. A line still being written at the end of the
// file is skipped.
func scanLinesBackward(path string, end int64, budget *int64, visit func(line []byte, lineStart int64) bool) (stop int64, stopped bool, err error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, false, err
	}
	defer file.Close()
	if end < 0 {
		info, err := file.Stat()
		if err != nil {
			return 0, false, err
		}
		end = info.Size()
	}
	for end > 0 {
		if *budget <= 0 {
			return end, true, nil
		}
		size := min(end, securityLogWindow, *budget)
		base := end - size
		window := make([]byte, size)
		if _, err := file.ReadAt(window, base); err != nil && !errors.Is(err, io.EOF) {
			return 0, false, err
		}
		*budget -= size
		// Start at the first whole line; the part before it is read with
		// the next window. Records are at most maxRecordBytes, far smaller
		// than a window, so a window always holds a line break.
		if base > 0 {
			cut := bytes.IndexByte(window, '\n')
			if cut < 0 {
				return 0, false, errors.New("audit line longer than the read window")
			}
			window, base = window[cut+1:], base+int64(cut+1)
		}
		// Keep complete lines only.
		window = window[:bytes.LastIndexByte(window, '\n')+1]
		for len(window) > 0 {
			cut := bytes.LastIndexByte(window[:len(window)-1], '\n')
			line, lineStart := window[cut+1:len(window)-1], base+int64(cut+1)
			window = window[:cut+1]
			if visit(line, lineStart) {
				return lineStart, true, nil
			}
		}
		end = base
	}
	return 0, false, nil
}

// logUse is what the security log shows about permission use: the role and
// endpoint pairs of allowed requests, how many request records were read and
// how far back they reach.
type logUse struct {
	used     map[[2]string]bool
	requests int
	oldest   time.Time
	complete bool
	searched []string
}

// permitUse reads the request records of the security log files newest
// first, at most scan bytes.
func permitUse(dir string, files []string, scan int64) (logUse, error) {
	use := logUse{used: map[[2]string]bool{}, searched: []string{}, complete: true}
	budget := scan
	for _, name := range files {
		if budget <= 0 {
			use.complete = false
			break
		}
		_, stopped, err := scanLinesBackward(filepath.Join(dir, name), -1, &budget, func(line []byte, _ int64) bool {
			if !bytes.Contains(line, rejectedKind) && !bytes.Contains(line, forwardedKind) {
				return false
			}
			var record auditRequestRecord
			if json.Unmarshal(line, &record) != nil || record.RequestID == "" {
				return false
			}
			use.requests++
			if use.oldest.IsZero() || record.Time.Before(use.oldest) {
				use.oldest = record.Time
			}
			if record.Decision == "forwarded" {
				for _, role := range record.Roles {
					use.used[[2]string{role, record.Operation}] = true
				}
			}
			return false
		})
		if errors.Is(err, os.ErrNotExist) {
			continue // archived since it was listed
		}
		if err != nil {
			return use, err
		}
		use.searched = append(use.searched, name)
		if stopped {
			use.complete = false
			break
		}
	}
	return use, nil
}

var (
	rejectedKind  = []byte(`"kind":"request.rejected"`)
	forwardedKind = []byte(`"kind":"request.forwarded"`)
)

// securityLogRecord decodes a request's final record and applies the
// filters. A request record that cannot be decoded is counted, not hidden.
func securityLogRecord(line []byte, query securityLogQuery, result *securityLogResult) (requestEvent, bool) {
	if !bytes.Contains(line, rejectedKind) && !bytes.Contains(line, forwardedKind) {
		return requestEvent{}, false
	}
	if query.ref != "" && !bytes.Contains(line, []byte(query.ref)) {
		return requestEvent{}, false
	}
	var record auditRequestRecord
	if err := json.Unmarshal(line, &record); err != nil || record.RequestID == "" {
		result.unreadable++
		return requestEvent{}, false
	}
	view := requestView(Event{
		Time: record.Time, RequestID: record.RequestID, Stage: record.Stage, Decision: record.Decision, Status: record.Status,
		Error: record.Error, Method: record.Method, Path: record.Path, Operation: record.Operation, API: record.API,
		Subject: record.Subject, Roles: record.Roles, ClientAddress: record.ClientAddress, Generation: record.Generation,
		DurationMS: record.DurationMS,
	})
	return view, query.matches(view)
}

// firstRecordID is the event ID of a file's first record, which names the
// file across a rotation; an empty file has none.
func firstRecordID(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	head := make([]byte, maxRecordBytes+1)
	n, err := io.ReadFull(file, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return "", err
	}
	line, _, _ := bytes.Cut(head[:n], []byte("\n"))
	var record struct {
		ID string `json:"event_id"`
	}
	if json.Unmarshal(line, &record) != nil {
		return "", nil
	}
	return record.ID, nil
}

func encodeSecurityLogCursor(c securityLogCursor) string {
	data, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(data)
}

func decodeSecurityLogCursor(text string) *securityLogCursor {
	data, err := base64.RawURLEncoding.DecodeString(text)
	var c securityLogCursor
	if err != nil || len(data) > 512 || decodeStrict(data, &c) != nil {
		return nil
	}
	if c.File == "" || filepath.Base(c.File) != c.File || strings.ContainsAny(c.File, `/\`) || c.Offset < -1 || len(c.First) > 128 {
		return nil
	}
	return &c
}

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
