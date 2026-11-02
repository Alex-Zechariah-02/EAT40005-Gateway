package gateway

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// writeAuditFile writes request records numbered from..to (oldest first) with
// an administrator record between them, as the writer would.
func writeAuditFile(t *testing.T, path string, from, to int, tail string) {
	t.Helper()
	var text strings.Builder
	base := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	for n := from; n <= to; n++ {
		kind, decision := "request.forwarded", "forwarded"
		if n%2 == 0 {
			kind, decision = "request.rejected", "reject"
		}
		line, _ := json.Marshal(Event{
			ID: fmt.Sprintf("e-%d", n), Time: base.Add(time.Duration(n) * time.Second), Kind: kind, RequestID: fmt.Sprintf("%032x", n),
			Stage: "FORWARD", Decision: decision, Status: 200, Method: "GET", Path: fmt.Sprintf("/a/%d", n), Operation: "read-a",
			Subject: fmt.Sprintf("user-%d", n%3), Roles: []string{"READER"},
		})
		text.Write(line)
		text.WriteString("\n" + `{"event_id":"admin-` + fmt.Sprint(n) + `","kind":"admin.read","path":"/admin/status"}` + "\n")
	}
	text.WriteString(tail)
	if err := os.WriteFile(path, []byte(text.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func refsOf(items []requestEvent) []string {
	refs := []string{}
	for _, item := range items {
		refs = append(refs, strings.TrimLeft(item.RequestID, "0"))
	}
	return refs
}

func TestSecurityLogSearch(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "audit.log")
	writeAuditFile(t, active+".2", 1, 4, "")
	writeAuditFile(t, active+".3", 5, 8, "")
	// The active file ends with a record still being written.
	writeAuditFile(t, active, 9, 12, `{"event_id":"e-13","kind":"request.forw`)
	files, missing, err := securityLogFiles(active)
	if err != nil || !slices.Equal(files, []string{"audit.log", "audit.log.3", "audit.log.2"}) || !slices.Equal(missing, []string{"audit.log.1"}) {
		t.Fatalf("files %v, missing %v (%v)", files, missing, err)
	}

	// Newest first across files; administrator records and the incomplete
	// line are not request results.
	all, err := searchSecurityLog(dir, files, nil, securityLogQuery{}, 100, securityLogPageScan)
	want := []string{"c", "b", "a", "9", "8", "7", "6", "5", "4", "3", "2", "1"}
	if err != nil || !slices.Equal(refsOf(all.items), want) || all.next != nil || all.unreadable != 0 {
		t.Fatalf("all: %v next %v unreadable %d (%v)", refsOf(all.items), all.next, all.unreadable, err)
	}
	if all.items[0].Decision != "refused" || all.items[1].Decision != "allowed" || all.items[1].Stage != "forward" {
		t.Errorf("projection: %+v", all.items[:2])
	}

	// Pages of three, and a tiny scan budget, both reach every record once.
	for _, tc := range []struct {
		limit int
		scan  int64
	}{{3, securityLogPageScan}, {100, 400}} {
		got := []string{}
		var cursor *securityLogCursor
		for page := 0; page < 50; page++ {
			result, err := searchSecurityLog(dir, files, cursor, securityLogQuery{}, tc.limit, tc.scan)
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, refsOf(result.items)...)
			if result.next == nil {
				break
			}
			cursor = decodeSecurityLogCursor(encodeSecurityLogCursor(*result.next))
		}
		if !slices.Equal(got, want) {
			t.Errorf("limit %d scan %d: %v", tc.limit, tc.scan, got)
		}
	}

	// Filters.
	for _, tc := range []struct {
		query securityLogQuery
		want  []string
	}{
		{securityLogQuery{ref: fmt.Sprintf("%032x", 7)}, []string{"7"}},
		{securityLogQuery{subject: "user-1"}, []string{"a", "7", "4", "1"}},
		{securityLogQuery{decision: "refused"}, []string{"c", "a", "8", "6", "4", "2"}},
		{securityLogQuery{from: time.Date(2026, 11, 2, 9, 0, 5, 0, time.UTC), to: time.Date(2026, 11, 2, 9, 0, 8, 0, time.UTC)}, []string{"7", "6", "5"}},
		{securityLogQuery{operation: "other"}, []string{}},
	} {
		result, err := searchSecurityLog(dir, files, nil, tc.query, 100, securityLogPageScan)
		if err != nil || !slices.Equal(refsOf(result.items), tc.want) {
			t.Errorf("%+v: %v (%v)", tc.query, refsOf(result.items), err)
		}
	}

	// A page that stopped in the active file resumes after a rotation renamed
	// it: the cursor finds the file by its first record.
	first, err := searchSecurityLog(dir, files, nil, securityLogQuery{}, 2, securityLogPageScan)
	if err != nil || first.next == nil || first.next.File != "audit.log" {
		t.Fatalf("first page: %+v (%v)", first.next, err)
	}
	if err := os.Rename(active, active+".4"); err != nil {
		t.Fatal(err)
	}
	writeAuditFile(t, active, 20, 21, "")
	files, _, _ = securityLogFiles(active)
	rest, err := searchSecurityLog(dir, files, first.next, securityLogQuery{}, 100, securityLogPageScan)
	if err != nil || !slices.Equal(refsOf(rest.items), want[2:]) {
		t.Errorf("after rotation: %v (%v)", refsOf(rest.items), err)
	}
	// A file moved away to archive ends that cursor clearly.
	os.Remove(active + ".4")
	files, _, _ = securityLogFiles(active)
	if _, err := searchSecurityLog(dir, files, first.next, securityLogQuery{}, 100, securityLogPageScan); err != errCursorGone {
		t.Errorf("archived cursor: %v", err)
	}

	// A damaged request record is counted, not hidden.
	os.WriteFile(active, []byte(`{"event_id":"x","kind":"request.rejected","time":"not a time"}`+"\n"), 0o600)
	if result, _ := searchSecurityLog(dir, []string{"audit.log"}, nil, securityLogQuery{}, 100, securityLogPageScan); result.unreadable != 1 || len(result.items) != 0 {
		t.Errorf("damaged record: %+v", result)
	}

	for _, text := range []string{"", "!!", encodeSecurityLogCursor(securityLogCursor{File: "../audit.log"}), encodeSecurityLogCursor(securityLogCursor{File: "audit.log", Offset: -2})} {
		if decodeSecurityLogCursor(text) != nil {
			t.Errorf("cursor %q accepted", text)
		}
	}
}

// Client-chosen values cannot run as spreadsheet formulas in an export.
func TestSecurityLogCSV(t *testing.T) {
	subject := "=HYPERLINK(1)"
	w := httptest.NewRecorder()
	writeSecurityLogCSV(w, securityLogResult{items: []requestEvent{{RequestID: "r", Path: "+1", Subject: &subject, Roles: []string{"-A"}}}, searched: []string{"audit.log"}}, nil, []string{})
	body := w.Body.String()
	if !strings.Contains(body, "'+1") || !strings.Contains(body, "'=HYPERLINK(1)") || !strings.Contains(body, "'-A") || w.Header().Get("X-Security-Log-Complete") != "true" {
		t.Errorf("csv: %s %v", body, w.Header())
	}
}

// The endpoint needs the history permission, reads the real audit files and
// says what it searched.
func TestAdminSecurityLog(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.plain)
	admin := e.mustLogin(harnessAdmin, harnessPassword)
	resp, _ := e.exchange("GET", "/a/x", "", "", "Authorization", "Bearer "+issuer.tokenFor(t, "bob"))
	ref := resp.Header.Get("X-Request-Id")
	e.get("/a/x")
	var page struct {
		Items         []map[string]any `json:"items"`
		Complete      bool             `json:"complete"`
		FilesSearched []string         `json:"files_searched"`
		FilesMissing  []string         `json:"files_missing"`
	}
	var code int
	var body string
	// The audit writer is asynchronous: wait for the record.
	for range 50 {
		code, body, _ = e.as(admin, "GET", "/admin/security-log?ref="+ref, "")
		if code == 200 && json.Unmarshal([]byte(body), &page) == nil && len(page.Items) == 1 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(page.Items) != 1 || page.Items[0]["subject"] != "bob" || page.Items[0]["decision"] != "allowed" || !page.Complete || len(page.FilesSearched) == 0 || page.FilesMissing == nil {
		t.Fatalf("search by ref: %d %s", code, body)
	}
	if strings.Contains(body, "Bearer") {
		t.Fatalf("credential in the results: %s", body)
	}
	code, csvBody, header := e.as(admin, "GET", "/admin/security-log?decision=refused&format=csv", "")
	if code != 200 || !strings.HasPrefix(header.Get("Content-Type"), "text/csv") || !strings.HasPrefix(csvBody, "time,request_id,decision") || !strings.Contains(csvBody, ",refused,401,") {
		t.Errorf("csv export: %d %s", code, csvBody)
	}
	for _, query := range []string{"limit=0", "limit=101", "decision=maybe", "from=yesterday", "after=!!", "format=xml"} {
		if code, body, _ := e.as(admin, "GET", "/admin/security-log?"+query, ""); code != 400 {
			t.Errorf("%s: %d %s", query, code, body)
		}
	}
	reader, _ := e.addAdmin(binaries.plain, "viewer", "read")
	if code, body, _ := e.as(reader, "GET", "/admin/security-log", ""); code != 403 {
		t.Errorf("without the history permission: %d %s", code, body)
	}
}
