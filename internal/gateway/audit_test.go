package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// auditRecords reads every record of the rotated audit files, oldest first,
// then of the active file, failing on any line that is not one JSON record.
func (e *env) auditRecords() []map[string]any {
	e.t.Helper()
	files, _ := filepath.Glob(e.auditLog + ".*")
	// Numeric order of the rotation suffix: shorter names first.
	slices.SortFunc(files, func(a, b string) int {
		if len(a) != len(b) {
			return len(a) - len(b)
		}
		return strings.Compare(a, b)
	})
	files = append(files, e.auditLog)
	var records []map[string]any
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			e.t.Fatal(err)
		}
		for line := range bytes.SplitSeq(bytes.TrimSuffix(data, []byte("\n")), []byte("\n")) {
			var record map[string]any
			if err := json.Unmarshal(line, &record); err != nil {
				e.t.Fatalf("%s holds a line that is not one record: %q", file, line)
			}
			records = append(records, record)
		}
	}
	return records
}

// Every decision class produces its own event with trusted attribution:
// a server timestamp, the captured generation, the operation, the verified
// subject only after verification, and the stages that actually ran.
// Publication events carry the transition and revision, and no event kind
// of an unimplemented component appears.
func TestEventsCoverEveryDecisionClass(t *testing.T) {
	e, fixtures := newFixtureEnv(t)
	e.start(binaries.fault) // publication below uses the test publication hook
	started := time.Now().UTC()

	tight := withRate(fixturePolicy(fixtures), rate(1, 60))
	published, code := e.publish(Change{ID: "tight", Expected: 1, Policy: mustJSON(tight)})
	if code != http.StatusOK {
		t.Fatalf("publish: %d %+v", code, published)
	}
	if rejected, code := e.publish(Change{ID: "broken", Expected: published.Revision, Policy: mustJSON(withRate(tight, rate(0, 60)))}); code != http.StatusBadRequest || rejected.Reason != "INVALID_VALUE" {
		t.Fatalf("invalid publish: %d %+v", code, rejected)
	}
	g := e.status().Generation

	staff := bearer(t, "staff-01")
	cases := []struct {
		name, method, path, body string
		header                   []string
		status                   int
		stage, reason, subject   string
		evaluated                []string // stages expected PASS before the terminal one
	}{
		{"authentication", "GET", "/orders", "", nil, 401, "JWT", "missing_credential", "", nil},
		{"authorisation", "GET", "/reports/sales", "", staff, 403, "RBAC", "no_permit", "staff-01", []string{"JWT"}},
		{"admitted under quota", "GET", "/orders", "", staff, 200, "", "", "staff-01", nil},
		{"rate", "GET", "/orders", "", staff, 429, "RATE", "quota_exceeded", "staff-01", []string{"JWT", "RBAC"}},
		{"validation", "PATCH", "/inventory/items/ITM-001", `{"delta":"x"}`, staff, 400, "VALIDATION", "schema_mismatch", "staff-01", []string{"JWT", "RBAC", "RATE", "BODY"}},
		{"routing", "GET", "/billing", "", staff, 404, "ROUTE", "", "", nil},
	}
	for _, tc := range cases {
		contentType := ""
		if tc.body != "" {
			contentType = "application/json"
		}
		resp, body := e.exchange(tc.method, tc.path, contentType, tc.body, tc.header...)
		if resp.StatusCode != tc.status {
			t.Fatalf("%s: %d %s", tc.name, resp.StatusCode, body)
		}
		if tc.status == 200 {
			continue
		}
		_, id := requestID(resp, body)
		events := e.requestEvents(id, 1)
		if len(events) != 1 {
			t.Fatalf("%s: %d events", tc.name, len(events))
		}
		ev := events[0]
		subject, _ := ev["subject"].(string)
		reason, _ := ev["reason"].(string)
		if ev["kind"] != "request.rejected" || ev["stage"] != tc.stage || reason != tc.reason || subject != tc.subject {
			t.Errorf("%s: event %v", tc.name, ev)
		}
		if tc.stage != "ROUTE" && ev["generation"] != g {
			t.Errorf("%s: generation %v, want %s", tc.name, ev["generation"], g)
		}
		stamp, err := time.Parse(time.RFC3339Nano, ev["time"].(string))
		if err != nil || !strings.HasSuffix(ev["time"].(string), "Z") || stamp.Before(started.Add(-time.Second)) || stamp.After(time.Now().Add(time.Second)) {
			t.Errorf("%s: time %v is not a current server UTC time", tc.name, ev["time"])
		}
		// Stages after the terminal one were not evaluated.
		stages := stagesOf(ev)
		after := false
		for _, name := range stageNames {
			if name == tc.stage {
				after = true
				continue
			}
			if after && stages[name] != stageNotRun {
				t.Errorf("%s: stage %s after the terminal stage reported %v", tc.name, name, stages[name])
			}
		}
		for _, name := range tc.evaluated {
			if stages[name] != stagePass {
				t.Errorf("%s: stage %s %v, want PASS", tc.name, name, stages[name])
			}
		}
	}

	events := e.auditEvents()
	kinds := map[string]bool{}
	var ids []string
	for _, ev := range events {
		kinds[ev["kind"].(string)] = true
		ids = append(ids, ev["event_id"].(string))
	}
	if len(slices.Compact(slices.Sorted(slices.Values(ids)))) != len(ids) {
		t.Fatal("event identities repeat")
	}
	for kind := range kinds {
		known := slices.Contains([]string{"request.admitted", "request.forwarded", "request.rejected", "controller.reconciled",
			"transition.intent", "transition.published", "transition.rejected", "admin.authenticated", "admin.read"}, kind)
		if !known {
			t.Errorf("unexpected event kind %s", kind)
		}
	}
	intent := findTransition(events, "transition.intent", "tight")
	done := findTransition(events, "transition.published", "tight")
	failed := findTransition(events, "transition.rejected", "broken")
	if intent == nil || done == nil || done["revision"] != float64(published.Revision) || done["generation"] != published.Generation ||
		done["actor"] != harnessAdmin || failed == nil || failed["reason"] != "INVALID_VALUE" {
		t.Fatalf("publication events: intent %v published %v rejected %v", intent, done, failed)
	}
}

func withRate(p Policy, r *RateRule) Policy {
	p.Operations = slices.Clone(p.Operations)
	for i := range p.Operations {
		if p.Operations[i].ID == "orders.list" {
			p.Operations[i].Rate = r
		}
	}
	return p
}

func findTransition(events []map[string]any, kind, id string) map[string]any {
	for _, ev := range events {
		if ev["kind"] == kind && ev["transition"] == id {
			return ev
		}
	}
	return nil
}

// Request bodies, header values and validator messages never reach the
// audit log, whether the request was refused or forwarded.
func TestBodiesAndHeaderValuesNeverRecorded(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.keywordPolicy(), seedOptions{})
	e.start(binaries.plain)

	secrets := []string{"BODY-SECRET-1", "BODY-SECRET-2", "HEADER-SECRET-3", "hunter2"}
	key := "Idempotency-Key: HEADER-SECRET-3\r\n"
	item := func(headers, body string) string {
		return fmt.Sprintf("POST /a/item HTTP/1.1\r\nHost: gw\r\nConnection: close\r\nContent-Type: application/json\r\nX-Api-Key: hunter2\r\n%sContent-Length: %d\r\n\r\n%s", headers, len(body), body)
	}
	for _, request := range []string{
		item(key, `{"kind":"alpha","count":1,"note":"BODY-SE"}`),               // forwarded
		item(key, `{"kind":"alpha","count":1,"password":"BODY-SECRET-1"}`),     // unknown field
		item(key, `{"kind":"BODY-SECRET-2","count":1}`),                        // enumeration
		item(key, `{"kind":"alpha","count":1,"note":"BODY-SECRET-1\n{"`),       // malformed
		item("Idempotency-Key: short\r\n", `{"kind":"alpha","count":1}`),       // header schema
		item(key, `{"kind":"alpha","count":1,"meta":{"tag":"BODY-SECRET-2"}}`), // nested
	} {
		e.rawExchange(request, false)
	}
	events := e.eventsWhen(func(events []map[string]any) bool { return countKind(events, "request.rejected") == 5 })
	if countKind(events, "request.rejected") != 5 || countKind(events, "request.forwarded") != 1 {
		t.Fatalf("events %v", events)
	}
	raw, _ := os.ReadFile(e.auditLog)
	for _, secret := range append(secrets, "BODY-SE", "password", "jsonschema", "minLength", "enum") {
		if bytes.Contains(raw, []byte(secret)) {
			t.Errorf("audit log contains %q", secret)
		}
	}
	e.assertReceipts(1, 0, 0)
}

// An admission is forwarded only after its record is durable: when the sync
// does not complete within the acknowledgement wait, or the process dies
// between write and sync, nothing is forwarded, and a record written but not
// acknowledged is never followed by a forwarded outcome.
func TestAdmissionWaitsForDurableAudit(t *testing.T) {
	t.Run("sync slower than the acknowledgement wait", func(t *testing.T) {
		e := newEnv(t)
		e.writeConfig(configOptions{audit: map[string]any{"audit_timeout": "1s"}})
		seed(t, e.database, e.policy(e.a, false), seedOptions{})
		e.start(binaries.fault, "GATEWAY_FAULT=audit-slow-sync")

		started := time.Now()
		resp, body := e.exchange(http.MethodGet, "/a/x", "", "")
		_, id := requestID(resp, body)
		if resp.StatusCode != http.StatusServiceUnavailable || time.Since(started) > 2500*time.Millisecond {
			t.Fatalf("slow sync: %d after %v", resp.StatusCode, time.Since(started))
		}
		e.assertReceipts(0, 0, 0)
		// The admission record becomes durable later; it is followed by the
		// AUDIT rejection, never by a forwarded outcome. The writer is still
		// in its delayed sync, so the rejection lands a few seconds later.
		var events []map[string]any
		for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
			events = nil
			for _, ev := range e.auditEvents() {
				if ev["request_id"] == id {
					events = append(events, ev)
				}
			}
			if len(events) >= 2 {
				break
			}
		}
		if len(events) != 2 || events[0]["kind"] != "request.admitted" || events[1]["kind"] != "request.rejected" || events[1]["stage"] != "AUDIT" {
			t.Fatalf("events %v", events)
		}
		if findEvent(e.auditEvents(), "request.forwarded", id) != nil {
			t.Fatal("an unacknowledged admission has a forwarded outcome")
		}
	})

	t.Run("process dies between write and sync", func(t *testing.T) {
		e := newEnv(t)
		seed(t, e.database, e.policy(e.a, false), seedOptions{})
		e.start(binaries.fault, "GATEWAY_FAULT=audit-crash-before-sync")

		req, _ := http.NewRequest(http.MethodGet, "http://"+e.dataAddr+"/a/x", http.NoBody)
		if resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req); err == nil {
			resp.Body.Close()
			t.Fatalf("request answered %d by a process that should have stopped", resp.StatusCode)
		}
		if code := e.waitExit(); code != crashExit {
			t.Fatalf("exit code %d", code)
		}
		e.assertReceipts(0, 0, 0)

		// After a restart the written record may be present, but nothing
		// claims it was forwarded, and admission works again.
		e.start(binaries.plain)
		if code, _ := e.get("/a/x"); code != http.StatusOK {
			t.Fatalf("after restart: %d", code)
		}
		records := e.auditRecords()
		admitted, forwarded := countKind(records, "request.admitted"), countKind(records, "request.forwarded")
		if admitted != 2 || forwarded != 1 {
			t.Fatalf("after the crash and one forwarded request: %d admitted, %d forwarded records", admitted, forwarded)
		}
		e.assertReceipts(1, 0, 0)
	})
}

// An interrupted append leaves an incomplete last record. After a restart it
// is kept, terminated, and named by an audit.recovered event; complete
// records are unchanged and admission works again.
func TestIncompleteRecordIsRecovered(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.policy(e.a, false), seedOptions{})
	e.start(binaries.plain)
	if code, _ := e.get("/a/x"); code != http.StatusOK {
		t.Fatal(code)
	}
	e.eventsWhen(func(events []map[string]any) bool { return countKind(events, "request.forwarded") == 1 })
	e.stop()
	complete, _ := os.ReadFile(e.auditLog)

	e.start(binaries.fault, "GATEWAY_FAULT=audit-partial")
	if code, _ := e.get("/a/x"); code != http.StatusServiceUnavailable {
		t.Fatalf("admission with a partial write: %d", code)
	}
	if !e.status().AuditFailed {
		t.Fatal("partial write not reported")
	}
	e.stop()
	partial, _ := os.ReadFile(e.auditLog)
	if !bytes.HasPrefix(partial, complete) || len(partial) == len(complete) || partial[len(partial)-1] == '\n' {
		t.Fatalf("expected an incomplete record after %d complete bytes; file has %d bytes", len(complete), len(partial))
	}
	offset := bytes.LastIndexByte(partial, '\n') + 1

	e.start(binaries.plain)
	if code, _ := e.get("/a/x"); code != http.StatusOK {
		t.Fatalf("after recovery: %d", code)
	}
	// The shared readers refuse any invalid line, so this one parses the file
	// itself and checks below that the interrupted record is the only one.
	var data []byte
	var marker map[string]any
	for deadline := time.Now().Add(5 * time.Second); marker == nil && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		data, _ = os.ReadFile(e.auditLog)
		for line := range bytes.SplitSeq(data, []byte("\n")) {
			var ev map[string]any
			if json.Unmarshal(line, &ev) == nil && ev["kind"] == "audit.recovered" {
				marker = ev
			}
		}
	}
	if !bytes.HasPrefix(data, partial) {
		t.Fatal("recovery rewrote existing records")
	}
	if marker == nil || marker["offset"] != float64(offset) || marker["file"] != filepath.Base(e.auditLog) {
		t.Fatalf("recovery event %v, want offset %d", marker, offset)
	}
	// The incomplete record is on its own line and is the only invalid one.
	invalid := 0
	for line := range bytes.SplitSeq(bytes.TrimSuffix(data, []byte("\n")), []byte("\n")) {
		if !json.Valid(line) {
			invalid++
			if !bytes.Equal(line, partial[offset:]) {
				t.Fatalf("invalid line %q is not the interrupted record", line)
			}
		}
	}
	if invalid != 1 {
		t.Fatalf("%d invalid lines, want 1", invalid)
	}
	e.assertReceipts(2, 0, 0)
}

// The active file is rotated before it would exceed its size, records are
// never lost or split, and when the retained files are full the gateway
// stops admitting rather than deleting evidence. An operator who archives
// the files and restarts restores admission.
func TestAuditRotationAndRetention(t *testing.T) {
	e := newEnv(t)
	e.writeConfig(configOptions{audit: map[string]any{"audit_max_bytes": 8192, "audit_max_files": 3}})
	seed(t, e.database, e.policy(e.a, false), seedOptions{})
	e.start(binaries.plain)

	// Concurrent clients until admission stops.
	var mu sync.Mutex
	refused := 0
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 200 {
				resp, body := e.exchange(http.MethodGet, "/a/x", "", "")
				mu.Lock()
				switch resp.StatusCode {
				case http.StatusOK:
				case http.StatusServiceUnavailable:
					refused++
				default:
					t.Errorf("status %d %s", resp.StatusCode, body)
				}
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	forwarded := e.a.receipts()
	if refused == 0 {
		t.Fatal("admission never stopped although retention was full")
	}
	if !e.status().AuditFailed {
		t.Fatal("full retention not reported")
	}
	e.stop()

	rotated, _ := filepath.Glob(e.auditLog + ".*")
	slices.Sort(rotated)
	if !slices.Equal(rotated, []string{e.auditLog + ".1", e.auditLog + ".2"}) {
		t.Fatalf("rotated files %v", rotated)
	}
	for _, file := range append(rotated, e.auditLog) {
		info, _ := os.Stat(file)
		// One batch may finish a file: at most 128 records of under 1 KiB.
		if info.Size() > 8192+128*1024 {
			t.Errorf("%s holds %d bytes", file, info.Size())
		}
	}

	// Every forwarded request has exactly one admission record across the
	// files, and each new file starts with a rotation marker naming its
	// predecessor.
	records := e.auditRecords()
	seen := map[string]int{}
	for _, r := range records {
		if r["kind"] == "request.admitted" {
			seen[r["request_id"].(string)]++
		}
	}
	for _, r := range forwarded {
		if seen[r.RequestID] != 1 {
			t.Fatalf("forwarded request %q has %d admission records", r.RequestID, seen[r.RequestID])
		}
	}
	for i, file := range []string{e.auditLog + ".2", e.auditLog} {
		data, _ := os.ReadFile(file)
		first, _, _ := bytes.Cut(data, []byte("\n"))
		var marker map[string]any
		json.Unmarshal(first, &marker)
		if marker["kind"] != "audit.rotated" || marker["file"] != filepath.Base(e.auditLog)+fmt.Sprintf(".%d", i+1) {
			t.Fatalf("%s starts with %s", file, first)
		}
	}

	// The operator archives the rotated files and restarts.
	archive := filepath.Join(e.dir, "archive")
	os.Mkdir(archive, 0o700)
	for _, file := range rotated {
		if err := os.Rename(file, filepath.Join(archive, filepath.Base(file))); err != nil {
			t.Fatal(err)
		}
	}
	e.start(binaries.plain)
	if code, _ := e.get("/a/x"); code != http.StatusOK || e.status().AuditFailed {
		t.Fatalf("after archiving: %d", code)
	}
	t.Logf("%d forwarded, %d refused once retention was full; %d records retained", len(forwarded), refused, len(records))
}

// A crash during rotation, after the rename, loses no acknowledged record:
// the renamed file holds them and the restarted gateway continues.
func TestCrashDuringRotation(t *testing.T) {
	e := newEnv(t)
	e.writeConfig(configOptions{audit: map[string]any{"audit_max_bytes": 4096, "audit_max_files": 4}})
	seed(t, e.database, e.policy(e.a, false), seedOptions{})
	e.start(binaries.fault, "GATEWAY_FAULT=audit-after-rename")

	for range 100 {
		req, _ := http.NewRequest(http.MethodGet, "http://"+e.dataAddr+"/a/x", http.NoBody)
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			break
		}
		resp.Body.Close()
	}
	if code := e.waitExit(); code != crashExit {
		t.Fatalf("exit code %d", code)
	}
	forwarded := e.a.receipts()
	if len(forwarded) == 0 {
		t.Fatal("no request was forwarded before the rotation")
	}
	if _, err := os.Stat(e.auditLog + ".1"); err != nil {
		t.Fatalf("renamed file: %v", err)
	}
	e.start(binaries.plain)
	if code, _ := e.get("/a/x"); code != http.StatusOK {
		t.Fatalf("after restart: %d", code)
	}
	records := e.auditRecords()
	admitted := map[string]bool{}
	for _, r := range records {
		if r["kind"] == "request.admitted" {
			admitted[r["request_id"].(string)] = true
		}
	}
	for _, r := range forwarded {
		if !admitted[r.RequestID] {
			t.Fatalf("forwarded request %s has no admission record after the crash", r.RequestID)
		}
	}
	e.assertReceipts(len(forwarded)+1, 0, 0)
}

// A failure to record an outcome after the upstream acted cannot undo that
// action: the fixture change stays, the log shows the admission without an
// outcome, the failure is reported, and no later request is admitted.
func TestOutcomeFailureStopsLaterAdmissions(t *testing.T) {
	fixtures := startFixtures(t)
	e := newEnv(t)
	seed(t, e.database, fixturePolicy(fixtures), seedOptions{})
	e.start(binaries.fault, "GATEWAY_FAULT=audit-fail-outcome")
	orders := fixtures["orders"]
	seedState := orders.state().SHA256

	resp, body := e.exchange(http.MethodPost, "/orders", "application/json", `{"customer":"cust-001","item":"ITM-001","quantity":1}`, bearer(t, "cust-001")...)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first order: %d %s", resp.StatusCode, body)
	}
	id := orders.receipts()[0].RequestID
	deadline := time.Now().Add(5 * time.Second)
	for !e.status().AuditFailed {
		if time.Now().After(deadline) {
			t.Fatal("outcome failure not reported")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if orders.state().SHA256 == seedState || len(orders.receipts()) != 1 {
		t.Fatal("the forwarded order did not take effect")
	}
	for _, path := range []string{"/orders", "/inventory/health"} {
		if code, _ := e.request(http.MethodGet, path, "", "", bearer(t, "staff-01")...); code != http.StatusServiceUnavailable {
			t.Errorf("GET %s after the outcome failure: %d, want 503", path, code)
		}
	}
	if code, _ := e.get("/orders"); code != http.StatusUnauthorized {
		t.Errorf("a refusal stays a refusal: %d", code)
	}
	if n := len(orders.receipts()); n != 1 {
		t.Fatalf("%d receipts after the failure, want 1", n)
	}
	events := e.auditEvents()
	if findEvent(events, "request.admitted", id) == nil || findEvent(events, "request.forwarded", id) != nil {
		t.Fatalf("log for %s: admitted %v forwarded %v", id, findEvent(events, "request.admitted", id), findEvent(events, "request.forwarded", id))
	}
	if !strings.Contains(e.output.String(), "audit log failed; protected admission disabled") {
		t.Fatalf("no operator diagnostic in\n%s", e.output)
	}
}

// Audit settings outside their bounds stop the gateway before any listener
// opens, rather than running with an unbounded or unusable audit file.
func TestAuditSettingsAtStartup(t *testing.T) {
	cases := []struct {
		name     string
		settings map[string]any
		cause    string
	}{
		{"file size below the bound", map[string]any{"audit_max_bytes": 1024}, "audit_max_bytes must be"},
		{"file size above the bound", map[string]any{"audit_max_bytes": 1 << 31}, "audit_max_bytes must be"},
		{"negative file size", map[string]any{"audit_max_bytes": -1}, "audit_max_bytes must be"},
		{"one file", map[string]any{"audit_max_files": 1}, "audit_max_files must be"},
		{"too many files", map[string]any{"audit_max_files": 1001}, "audit_max_files must be"},
		{"unknown setting", map[string]any{"audit_delete_oldest": true}, `unknown field \"audit_delete_oldest\"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.writeConfig(configOptions{audit: tc.settings})
			seed(t, e.database, e.policy(e.a, false), seedOptions{})
			e.launch(binaries.plain, []string{"run", "--config", e.config})
			if code := e.waitExit(); code == 0 {
				t.Fatalf("gateway ran with %v\n%s", tc.settings, e.output)
			}
			if !strings.Contains(e.output.String(), tc.cause) {
				t.Fatalf("refused for another reason, want %q:\n%s", tc.cause, e.output)
			}
			if dialable(e.dataAddr) || dialable(e.admin) {
				t.Fatal("a listener is open")
			}
		})
	}
}

// The writer's queue is bounded: with the writer unable to keep up, an
// admission that cannot be queued is refused at once, and one that is queued
// but never written times out; neither is acknowledged.
func TestAuditQueueIsBounded(t *testing.T) {
	a := &auditLog{boot: "test", queue: make(chan auditEntry, 1), done: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := a.Commit(ctx, Event{Kind: "request.admitted"}); !errors.Is(err, errAuditFailed) || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("queued but unwritten: %v", err)
	}
	if err := a.Commit(context.Background(), Event{Kind: "request.admitted"}); !errors.Is(err, errAuditFailed) || !strings.Contains(err.Error(), "queue full") {
		t.Fatalf("queue full: %v", err)
	}
	a.Record(Event{Kind: "request.rejected"})
	if a.lost.Load() != 1 {
		t.Fatalf("lost %d, want 1", a.lost.Load())
	}
}
