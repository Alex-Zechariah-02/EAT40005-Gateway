// Audit rotation, retention, settings and queue bound.

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
