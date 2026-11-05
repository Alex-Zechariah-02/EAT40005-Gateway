// Backup helpers, backup with WAL and backup refusals.

package gateway

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Alex-Zechariah-02/apiward/internal/store"
)

// backup runs the backup command and returns the backup's manifest.
func (e *env) backup(output string) backupManifest {
	e.t.Helper()
	out, code := e.runOperator(binaries.plain, nil, "backup", "--output", output)
	if code != 0 {
		e.t.Fatalf("backup exited %d: %s", code, out)
	}
	data, err := os.ReadFile(manifestPath(output))
	if err != nil {
		e.t.Fatal(err)
	}
	return fromJSON[backupManifest](e.t, string(data))
}

// recoverAdministrator enrols root on a stopped gateway, starts it and logs
// root in with every permission.
func (e *env) recoverAdministrator() adminSession {
	e.t.Helper()
	e.stop()
	out, code := e.runOperator(binaries.plain, nil, "enrol", "--admin", "root", "--permissions", "read,read_history,manage_subjects,manage_policy")
	if code != 0 {
		e.t.Fatalf("enrol exited %d: %s", code, out)
	}
	e.start(binaries.plain)
	password := testPassword()
	if code, body := e.enrol("root", enrolmentSecret(e.t, out), password); code != http.StatusCreated {
		e.t.Fatalf("enrolment: %d %s", code, body)
	}
	return e.mustLogin("root", password)
}

// establishRecoveryBase stages a deny-all candidate and confirms it as the
// recovery base.
func (e *env) establishRecoveryBase(s adminSession) Outcome {
	e.t.Helper()
	code, staged, body := e.stage(s, e.denyAllPolicy(), `"r0"`, "recovery-deny-all")
	if code != http.StatusCreated || staged.Candidate.Parent != 0 || !staged.Candidate.DenyAll {
		e.t.Fatalf("staging the recovery base: %d %s", code, body)
	}
	code, outcome := e.establish(s, "/admin/recovery-base", staged.Candidate.ID, staged.Candidate.Checksum)
	if code != http.StatusOK || !strings.HasPrefix(outcome.Transition, "recovery:") {
		e.t.Fatalf("recovery base: %d %+v", code, outcome)
	}
	return outcome
}

func (e *env) historyItems(s adminSession) []store.HistoryEntry {
	e.t.Helper()
	code, body, _ := e.as(s, http.MethodGet, "/admin/history?limit=100", "")
	if code != http.StatusOK {
		e.t.Fatalf("history: %d %s", code, body)
	}
	return fromJSON[struct {
		Items []store.HistoryEntry `json:"items"`
	}](e.t, body).Items
}

// A backup taken after the gateway stopped abruptly includes the
// changes still in the write-ahead log, and restores in an isolated location
// into a recovery that keeps the history and trusts nothing restored.
func TestBackupIncludesWALAndRestoresElsewhere(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	e.start(binaries.plain)
	e.confirmPolicy(e.mustLogin(harnessAdmin, harnessPassword), withoutRole(e.protectedPolicy(), "WRITER"), 1, "change-before-backup")
	e.stop()

	// The change is only in the write-ahead log: the main file alone lacks it.
	info, err := os.Stat(e.database + "-wal")
	if err != nil || info.Size() == 0 {
		t.Fatalf("no write-ahead log after the abrupt stop: %v", err)
	}
	mainOnly := filepath.Join(t.TempDir(), "main-only.db")
	copyFile(t, e.database, mainOnly)
	if n := queryInt(t, mainOnly, `SELECT COUNT(*) FROM revision`); n != 1 {
		t.Fatalf("the main file alone holds %d revisions; the test needs the change to be in the log only", n)
	}

	output := filepath.Join(e.dir, "backup.db")
	manifest := e.backup(output)
	if manifest.Revision != 2 || manifest.Mode != store.ModeNormal || manifest.SchemaVersion != store.DatabaseVersion || manifest.Format != backupFormat {
		t.Fatalf("manifest %+v", manifest)
	}
	if manifest.SHA256 != fileSHA256(t, output) || manifest.Installation != queryString(t, e.database, `SELECT installation FROM deployment`) {
		t.Fatalf("manifest does not describe the backup: %+v", manifest)
	}
	if n := queryInt(t, output, `SELECT COUNT(*) FROM revision`); n != 2 {
		t.Fatalf("backup holds %d revisions, want 2", n)
	}
	if mode := queryString(t, output, `PRAGMA journal_mode`); mode != "delete" {
		t.Fatalf("backup journal mode %q", mode)
	}
	if _, err := os.Stat(output + ".partial"); err == nil {
		t.Fatal("the temporary backup file remains")
	}
	backupHash := fileSHA256(t, output)
	backups := 0
	for _, ev := range e.auditEvents() {
		if ev["kind"] == "operator.backup" && ev["actor"] == "operator" && ev["revision"] == float64(2) {
			backups++
		}
	}
	if backups != 1 {
		t.Fatalf("%d operator.backup events", backups)
	}

	// Restore into a separate deployment.
	other := newEnv(t)
	out, code := other.runOperator(binaries.plain, nil, "restore", "--backup", output)
	if code != 0 || !strings.Contains(out, "recovery_required") {
		t.Fatalf("restore exited %d: %s", code, out)
	}
	if fileSHA256(t, output) != backupHash {
		t.Fatal("restore changed the backup")
	}
	for query, want := range map[string]int{
		`SELECT COUNT(*) FROM revision`:                                                           2,
		`SELECT COUNT(*) FROM administrator WHERE status = 'active'`:                              0,
		`SELECT COUNT(*) FROM administrator WHERE status = 'quarantined'`:                         1,
		`SELECT COUNT(*) FROM enrolment`:                                                          0,
		`SELECT COUNT(*) FROM deployment WHERE mode = 'recovery_required'`:                        1,
		`SELECT COUNT(*) FROM transition WHERE kind = 'operator.restore'`:                         1,
		`SELECT COUNT(*) FROM transition WHERE id = 'admin:harness:confirm-change-before-backup'`: 1,
	} {
		if n := queryInt(t, other.database, query); n != want {
			t.Errorf("%s = %d, want %d", query, n, want)
		}
	}

	other.start(binaries.plain)
	if health := other.health(); health != store.ModeRecovery {
		t.Fatalf("health %q", health)
	}
	carol := issuer.tokenFor(t, "carol")
	if code, _ := other.get("/a/x", "Authorization", "Bearer "+carol); code != http.StatusServiceUnavailable {
		t.Fatalf("restored subject before the recovery base: %d", code)
	}
	if _, code := other.login(harnessAdmin, harnessPassword); code != http.StatusUnauthorized {
		t.Fatalf("restored administrator logs in: %d", code)
	}

	root := other.recoverAdministrator()
	found := false
	for _, entry := range other.historyItems(root) {
		found = found || (entry.ID == "admin:harness:confirm-change-before-backup" && entry.Revision == 2 && entry.Actor == harnessAdmin)
	}
	if !found {
		t.Fatal("the change that was in the write-ahead log is missing from the restored history")
	}
	recovered := other.establishRecoveryBase(root)
	if recovered.Revision != 3 || other.health() != store.ModeNormal {
		t.Fatalf("recovery base %+v", recovered)
	}
	if code, _ := other.get("/a/x", "Authorization", "Bearer "+carol); code != http.StatusForbidden {
		t.Fatalf("restored subject after the deny-all recovery base: %d", code)
	}
	other.assertReceipts(0, 0, 0)
	other.stop()
	if origin := queryString(t, other.database, `SELECT origin FROM revision WHERE id = 3`); origin != "recovery_base" {
		t.Fatalf("recovery base origin %q", origin)
	}
}

// An existing output is never replaced, and an interrupted or
// failed backup leaves no file that looks complete.
func TestBackupRefusalsAndInterruption(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	liveRevisions := queryInt(t, e.database, `SELECT COUNT(*) FROM revision`)

	existing := filepath.Join(e.dir, "existing.db")
	if err := os.WriteFile(existing, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, code := e.runOperator(binaries.plain, nil, "backup", "--output", existing); code == 0 || !strings.Contains(out, "already exists") {
		t.Fatalf("backup over an existing file: %d %s", code, out)
	}
	if data, err := os.ReadFile(existing); err != nil || string(data) != "keep me" {
		t.Fatal("the existing file was changed")
	}

	unwritable := filepath.Join(e.dir, "no-such-directory", "backup.db")
	if out, code := e.runOperator(binaries.plain, nil, "backup", "--output", unwritable); code == 0 {
		t.Fatalf("backup into a missing directory: %s", out)
	}
	if _, err := os.Stat(manifestPath(unwritable)); err == nil {
		t.Fatal("a failed backup wrote a manifest")
	}

	interrupted := filepath.Join(e.dir, "interrupted.db")
	if _, code := e.runOperator(binaries.fault, []string{"GATEWAY_FAULT=backup-before-verify"}, "backup", "--output", interrupted); code != crashExit {
		t.Fatalf("interrupted backup exit code %d", code)
	}
	for path, present := range map[string]bool{interrupted + ".partial": true, interrupted: false, manifestPath(interrupted): false} {
		if _, err := os.Stat(path); (err == nil) != present {
			t.Errorf("%s present = %v, want %v", filepath.Base(path), err == nil, present)
		}
	}
	// A restore of the incomplete backup is refused, and backing up again
	// replaces the stale temporary file.
	if out, code := e.runOperator(binaries.plain, nil, "restore", "--backup", interrupted); code == 0 {
		t.Fatalf("restore of an interrupted backup: %s", out)
	}
	manifest := e.backup(interrupted)
	if manifest.SHA256 != fileSHA256(t, interrupted) {
		t.Fatal("the repeated backup does not match its manifest")
	}
	if _, err := os.Stat(interrupted + ".partial"); err == nil {
		t.Fatal("the stale temporary file remains")
	}

	if n := queryInt(t, e.database, `SELECT COUNT(*) FROM revision`); n != liveRevisions {
		t.Fatalf("live database revisions %d, want %d", n, liveRevisions)
	}
	if mode := queryString(t, e.database, `SELECT mode FROM deployment`); mode != store.ModeNormal {
		t.Fatalf("live mode %q", mode)
	}
	e.start(binaries.plain)
	if code, _ := e.get("/a/x", "Authorization", "Bearer "+issuer.tokenFor(t, "alice")); code != http.StatusOK {
		t.Fatalf("live gateway after the backup failures: %d", code)
	}
}
