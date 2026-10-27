package gateway

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func (e *env) historyItems(s adminSession) []historyEntry {
	e.t.Helper()
	code, body, _ := e.as(s, http.MethodGet, "/admin/history?limit=100", "")
	if code != http.StatusOK {
		e.t.Fatalf("history: %d %s", code, body)
	}
	return fromJSON[struct {
		Items []historyEntry `json:"items"`
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
	if manifest.Revision != 2 || manifest.Mode != modeNormal || manifest.SchemaVersion != databaseVersion || manifest.Format != backupFormat {
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
	if health := other.health(); health != modeRecovery {
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
	if recovered.Revision != 3 || other.health() != modeNormal {
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
	if health := e.health(); health != modeRecovery {
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
	if health := e.health(); health != modeNormal {
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
		{"manifest with another schema version", seal(write("future.db", goodData, nil), func(m *backupManifest) { m.SchemaVersion = databaseVersion + 1 }), fmt.Sprintf("schema version %d", databaseVersion+1)},
		{"database with another schema version", alter("version.db", fmt.Sprintf(`PRAGMA user_version = %d`, databaseVersion+1)), fmt.Sprintf("schema version %d", databaseVersion+1)},
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
	if e.health() != modeNormal {
		t.Fatal("the refused restore changed the running gateway")
	}
	e.stop()
	if mode := queryString(t, e.database, `SELECT mode FROM deployment`); mode != modeNormal {
		t.Fatalf("live mode %q after the refusals", mode)
	}
	leftovers, _ := filepath.Glob(e.database + ".re*")
	if len(leftovers) != 0 {
		t.Fatalf("refused restores left files: %v", leftovers)
	}
}

// A backup taken while a provisional base was active, whether its
// deadline has passed by the time of the restore or not, never brings that
// provisional authority back: the restore closes its activation as
// quarantined (not confirmed, not rolled back), nothing is served until a new
// deny-all recovery base is confirmed, and that base has no provisional base.
func TestHistoricalRestoreQuarantinesProvisional(t *testing.T) {
	for _, tc := range []struct {
		name    string
		seconds int64
		expired bool // restore after the deadline has passed
	}{{"deadline not yet passed", 3600, false}, {"deadline passed", 2, true}} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			seed(t, e.database, e.protectedPolicy(), seedOptions{})
			e.start(binaries.plain)
			harness := e.mustLogin(harnessAdmin, harnessPassword)
			_, activated := e.provisionalFrom(harness, e.cpaCandidate(), 1, tc.seconds, "restore-provisional")
			e.stop()
			backup := filepath.Join(e.dir, "provisional.db")
			e.backup(backup)
			if n := queryInt(t, backup, `SELECT COUNT(*) FROM base WHERE provisional = 2`); n != 1 {
				t.Fatal("the backup does not hold the provisional base")
			}
			if tc.expired {
				time.Sleep(time.Until(activated.Deadline.Add(200 * time.Millisecond)))
			}

			if out, code := e.runOperator(binaries.plain, nil, "restore", "--backup", backup); code != 0 {
				t.Fatalf("restore exited %d: %s", code, out)
			}
			list := storedActivations(t, e.database)
			if len(list) != 1 || list[0].Disposition != dispositionQuarantined || list[0].ResolvedBy != "operator" ||
				!strings.HasPrefix(list[0].Resolution, "operator:restore:") {
				t.Fatalf("restored activations %+v", list)
			}
			if n := queryInt(t, e.database, `SELECT COUNT(*) FROM base WHERE provisional IS NULL AND deadline IS NULL AND confirmed = 1`); n != 1 {
				t.Fatal("the restored base still points at the provisional revision")
			}

			e.start(binaries.plain)
			if health := e.health(); health != modeRecovery {
				t.Fatalf("health %q", health)
			}
			e.expectNothingServed()
			root := e.recoverAdministrator()
			e.expectNothingServed()
			recovery := e.establishRecoveryBase(root)
			// Restored administrators, the harness's among them, stay
			// quarantined: only the re-enrolled root can read the status.
			code, body, _ := e.as(root, http.MethodGet, "/admin/status", "")
			if s := fromJSON[status](t, body); code != http.StatusOK || s.Provisional != 0 || s.Confirmed != recovery.Revision || !s.NotAfter.IsZero() {
				t.Fatalf("status after the recovery base: %d %s", code, body)
			}
			e.expectNothingServed()
			// The quarantined revision cannot be confirmed or rolled back.
			for _, action := range []string{"confirm", "rollback"} {
				code, outcome, body := e.resolve(root, action, 2, policyETag(recovery.Revision), "restored-"+action+"-xxxxx")
				if code != http.StatusConflict || outcome.Reason != "ALREADY_RESOLVED" || outcome.Disposition != dispositionQuarantined {
					t.Fatalf("%s of the quarantined revision: %d %s", action, code, body)
				}
			}
			code, body, _ = e.as(root, http.MethodGet, "/admin/activations", "")
			if items := fromJSON[struct {
				Items []activationRecord `json:"items"`
			}](t, body).Items; code != http.StatusOK || len(items) != 1 || items[0].Disposition != dispositionQuarantined {
				t.Fatalf("activation history: %d %s", code, body)
			}
		})
	}
}

// expectNothingServed checks that no protected or public probe reaches any
// receiver.
func (e *env) expectNothingServed() {
	e.t.Helper()
	e.resetReceivers()
	bobToken := "Bearer " + issuer.tokenFor(e.t, "bob")
	for _, probe := range []struct{ method, path string }{{"GET", "/b/x"}, {"POST", "/a/item"}, {"GET", "/a/x"}, {"GET", "/d"}} {
		code, _ := e.request(probe.method, probe.path, "application/json", `{}`, "Authorization", bobToken)
		if code == http.StatusOK {
			e.t.Fatalf("%s %s admitted", probe.method, probe.path)
		}
	}
	e.assertReceipts(0, 0, 0)
}

// A crash at each step of a restore leaves
// the original database, no database or the marked copy, never an unmarked
// historical one, and running the restore again completes it.
func TestRestoreSurvivesCrash(t *testing.T) {
	for _, tc := range []struct {
		point  string
		health string // after the crash, before the restore is repeated
	}{
		{"restore-before-promote", modeNormal},
		{"restore-after-retire", "unavailable"},
		{"restore-after-promote", modeRecovery},
	} {
		t.Run(tc.point, func(t *testing.T) {
			e := newEnv(t)
			seed(t, e.database, e.protectedPolicy(), seedOptions{})
			older := filepath.Join(e.dir, "older.db")
			e.backup(older)

			if _, code := e.runOperator(binaries.fault, []string{"GATEWAY_FAULT=" + tc.point}, "restore", "--backup", older); code != crashExit {
				t.Fatalf("restore exit code %d", code)
			}
			if staged, err := os.Stat(e.database + ".restore"); err == nil && staged.Size() > 0 {
				if mode := queryString(t, e.database+".restore", `SELECT mode FROM deployment`); mode != modeRecovery {
					t.Fatalf("the staged copy is not marked: %q", mode)
				}
			}

			e.start(binaries.plain)
			if health := e.health(); health != tc.health {
				t.Fatalf("health after the crash %q, want %q", health, tc.health)
			}
			want := http.StatusServiceUnavailable
			if tc.health == modeNormal {
				want = http.StatusOK
			}
			if code, _ := e.get("/a/x", "Authorization", "Bearer "+issuer.tokenFor(t, "alice")); code != want {
				t.Fatalf("data plane after the crash: %d, want %d", code, want)
			}
			if tc.point == "restore-after-promote" {
				// The lost outcome event is recorded from the database.
				reconciled := false
				for _, ev := range e.eventsWhen(func(events []map[string]any) bool { return countKind(events, "controller.unavailable") > 0 }) {
					id, _ := ev["transition"].(string)
					reconciled = reconciled || (ev["kind"] == "transition.committed" && ev["reason"] == "reconciled" && strings.HasPrefix(id, "operator:restore:"))
				}
				if !reconciled {
					t.Fatal("no reconciled outcome event for the promoted restore")
				}
				e.stop()
				if n := queryInt(t, e.database, `SELECT COUNT(*) FROM transition WHERE kind = 'operator.restore' AND audited_at IS NOT NULL`); n != 1 {
					t.Fatalf("%d audited restore transitions", n)
				}
				return
			}
			e.stop()

			out, code := e.runOperator(binaries.plain, nil, "restore", "--backup", older)
			if code != 0 {
				t.Fatalf("repeated restore exited %d: %s", code, out)
			}
			e.start(binaries.plain)
			if health := e.health(); health != modeRecovery {
				t.Fatalf("health after the repeated restore %q", health)
			}
			e.stop()
			if _, err := os.Stat(e.database + ".restore"); err == nil {
				t.Fatal("the staging file remains")
			}
		})
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
	if mode := queryString(t, e.database, `SELECT mode FROM deployment`); mode != modeNormal {
		t.Fatalf("live mode %q", mode)
	}
	e.start(binaries.plain)
	if code, _ := e.get("/a/x", "Authorization", "Bearer "+issuer.tokenFor(t, "alice")); code != http.StatusOK {
		t.Fatalf("live gateway after the backup failures: %d", code)
	}
}
