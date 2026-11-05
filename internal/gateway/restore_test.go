// Historical restore and restore refusals.

package gateway

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Alex-Zechariah-02/apiward/internal/store"
)

// Restoring an older backup over a newer database of the same
// installation is a declared historical recovery. The newer database is kept
// aside, nothing restored is trusted, and nothing is admitted until an
// enrolled administrator confirms a deny-all recovery base.
func TestHistoricalRestore(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.plain)
	harness := e.mustLogin(harnessAdmin, harnessPassword)
	code, staged, body := e.stage(harness, e.denyAllPolicy(), `"r1"`, "candidate-before-backup")
	if code != http.StatusCreated {
		t.Fatalf("staging: %d %s", code, body)
	}
	e.stop()
	older := filepath.Join(e.dir, "older.db")
	e.backup(older)

	// Newer state: alice is removed after the backup.
	e.start(binaries.plain)
	harness = e.mustLogin(harnessAdmin, harnessPassword)
	e.confirmPolicy(harness, withoutRole(e.protectedPolicy(), "WRITER"), 1, "remove-writer-later")
	e.stop()

	out, code := e.runOperator(binaries.plain, nil, "restore", "--backup", older)
	if code != 0 {
		t.Fatalf("restore exited %d: %s", code, out)
	}
	replaced, _ := filepath.Glob(e.database + ".replaced-*")
	if len(replaced) != 1 {
		t.Fatalf("replaced databases %v", replaced)
	}
	newerHash := fileSHA256(t, replaced[0])
	if n := queryInt(t, replaced[0], `SELECT COUNT(*) FROM revision`); n != 2 {
		t.Fatalf("the kept newer database holds %d revisions", n)
	}
	for _, companion := range []string{e.database + "-wal", e.database + "-shm", e.database + ".restore"} {
		if _, err := os.Stat(companion); err == nil {
			t.Errorf("%s remains after the restore", companion)
		}
	}
	if status := queryString(t, e.database, `SELECT status FROM candidate WHERE id = ?`, staged.Candidate.ID); status != "quarantined" {
		t.Fatalf("restored candidate status %q", status)
	}

	e.start(binaries.plain)
	aliceToken := issuer.tokenFor(t, "alice")
	if health := e.health(); health != store.ModeRecovery {
		t.Fatalf("health %q", health)
	}
	// alice is active with WRITER in the restored policy, yet not admitted.
	if code, _ := e.request(http.MethodPost, "/a/item", "application/json", `{"n":1}`, "Authorization", "Bearer "+aliceToken); code != http.StatusServiceUnavailable {
		t.Fatalf("restored subject during recovery: %d", code)
	}
	if _, code := e.login(harnessAdmin, harnessPassword); code != http.StatusUnauthorized {
		t.Fatalf("restored administrator during recovery: %d", code)
	}

	root := e.recoverAdministrator()
	if code, outcome := e.establish(root, "/admin/recovery-base", staged.Candidate.ID, staged.Candidate.Checksum); code != http.StatusConflict || outcome.Reason != "CANDIDATE_UNAVAILABLE" {
		t.Fatalf("recovery base from a quarantined candidate: %d %+v", code, outcome)
	}
	if code, outcome := e.establish(root, "/admin/genesis", staged.Candidate.ID, staged.Candidate.Checksum); code != http.StatusConflict || outcome.Reason != "INVALID_STATE" {
		t.Fatalf("genesis during recovery: %d %+v", code, outcome)
	}
	code, body, _ = e.as(root, http.MethodPut, "/admin/blocked-accounts/carol", `{"reason":"test containment"}`, "If-Match", `"r1"`, "Idempotency-Key", "membership-in-recovery")
	if code != http.StatusOK || !strings.Contains(body, "carol") {
		t.Fatalf("independent containment during recovery: %d %s", code, body)
	}
	e.establishRecoveryBase(root)
	if health := e.health(); health != store.ModeNormal {
		t.Fatalf("health after the recovery base %q", health)
	}
	if code, _ := e.request(http.MethodPost, "/a/item", "application/json", `{"n":1}`, "Authorization", "Bearer "+aliceToken); code != http.StatusForbidden {
		t.Fatalf("restored subject after the recovery base: %d", code)
	}
	e.assertReceipts(0, 0, 0)
	kinds := map[string]bool{}
	for _, entry := range e.historyItems(root) {
		kinds[entry.Kind] = true
	}
	if !kinds["operator.restore"] || !kinds["recovery_base"] || !kinds["admin.enrol"] {
		t.Fatalf("history kinds %v", kinds)
	}
	e.stop()
	if fileSHA256(t, replaced[0]) != newerHash {
		t.Fatal("the kept newer database changed")
	}
}

// Invalid backups and unsafe restores are refused and the live
// database is left exactly as it was.
func TestRestoreRefusals(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	good := filepath.Join(e.dir, "good.db")
	e.backup(good)
	goodData, _ := os.ReadFile(good)
	goodManifest, _ := os.ReadFile(manifestPath(good))

	// Each case builds a backup file and its manifest.
	write := func(name string, data []byte, manifest []byte) string {
		path := filepath.Join(e.dir, name)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if manifest != nil {
			if err := os.WriteFile(manifestPath(path), manifest, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return path
	}
	// seal writes a manifest that matches the file, with an optional change,
	// so only the database checks can refuse the file.
	seal := func(path string, change func(*backupManifest)) string {
		m := fromJSON[backupManifest](t, string(goodManifest))
		m.SHA256 = fileSHA256(t, path)
		info, _ := os.Stat(path)
		m.Bytes = info.Size()
		if change != nil {
			change(&m)
		}
		if err := os.WriteFile(manifestPath(path), mustJSON(m), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	alter := func(name string, statement string) string {
		path := write(name, goodData, nil)
		db := inspect(t, path)
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
		db.Close()
		return seal(path, nil)
	}

	flipped := append([]byte(nil), goodData...)
	flipped[len(flipped)/2] ^= 0xff
	foreign := newEnv(t)
	seed(t, foreign.database, foreign.protectedPolicy(), seedOptions{})
	otherInstallation := filepath.Join(foreign.dir, "foreign.db")
	foreign.backup(otherInstallation)

	cases := []struct {
		name, backup, refusal string
	}{
		{"flipped byte", write("flipped.db", flipped, goodManifest), "does not match its manifest"},
		{"truncated", write("truncated.db", goodData[:len(goodData)/2], goodManifest), "does not match its manifest"},
		{"missing manifest", write("bare.db", goodData, nil), "reading the backup manifest"},
		{"not a manifest", write("odd.db", goodData, []byte(`{"format":"other"}`)), "not a gateway backup manifest"},
		{"manifest with another schema version", seal(write("future.db", goodData, nil), func(m *backupManifest) { m.SchemaVersion = store.DatabaseVersion + 1 }), fmt.Sprintf("schema version %d", store.DatabaseVersion+1)},
		{"database with another schema version", alter("version.db", fmt.Sprintf(`PRAGMA user_version = %d`, store.DatabaseVersion+1)), fmt.Sprintf("schema version %d", store.DatabaseVersion+1)},
		{"altered schema", alter("altered.db", `CREATE INDEX extra ON revision (actor)`), "schema differs"},
		{"another installation", otherInstallation, "belongs to installation"},
		{"the live database itself", e.database, "cannot be the live database"},
	}

	liveHash := fileSHA256(t, e.database)
	for _, tc := range cases {
		out, code := e.runOperator(binaries.plain, nil, "restore", "--backup", tc.backup)
		if code == 0 || !strings.Contains(out, tc.refusal) {
			t.Errorf("%s: exit %d: %s", tc.name, code, out)
		}
		if fileSHA256(t, e.database) != liveHash {
			t.Fatalf("%s changed the live database", tc.name)
		}
	}

	// A running gateway's database is never replaced.
	e.start(binaries.plain)
	out, code := e.runOperator(binaries.plain, nil, "restore", "--backup", good)
	if code == 0 || !strings.Contains(out, "held by another gateway process") {
		t.Errorf("restore beside a running gateway: %d %s", code, out)
	}
	if e.health() != store.ModeNormal {
		t.Fatal("the refused restore changed the running gateway")
	}
	e.stop()
	if mode := queryString(t, e.database, `SELECT mode FROM deployment`); mode != store.ModeNormal {
		t.Fatalf("live mode %q after the refusals", mode)
	}
	leftovers, _ := filepath.Glob(e.database + ".re*")
	if len(leftovers) != 0 {
		t.Fatalf("refused restores left files: %v", leftovers)
	}
}
