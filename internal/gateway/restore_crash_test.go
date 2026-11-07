// Restore quarantine and crash during restore.

package gateway

import (
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/store"
)

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
			if len(list) != 1 || list[0].Disposition != store.DispositionQuarantined || list[0].ResolvedBy != "operator" ||
				!strings.HasPrefix(list[0].Resolution, "operator:restore:") {
				t.Fatalf("restored activations %+v", list)
			}
			if n := queryInt(t, e.database, `SELECT COUNT(*) FROM base WHERE provisional IS NULL AND deadline IS NULL AND confirmed = 1`); n != 1 {
				t.Fatal("the restored base still points at the provisional revision")
			}

			e.start(binaries.plain)
			if health := e.health(); health != store.ModeRecovery {
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
				if code != http.StatusConflict || outcome.Reason != "ALREADY_RESOLVED" || outcome.Disposition != store.DispositionQuarantined {
					t.Fatalf("%s of the quarantined revision: %d %s", action, code, body)
				}
			}
			code, body, _ = e.as(root, http.MethodGet, "/admin/activations", "")
			if items := fromJSON[struct {
				Items []store.ActivationRecord `json:"items"`
			}](t, body).Items; code != http.StatusOK || len(items) != 1 || items[0].Disposition != store.DispositionQuarantined {
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
		{"restore-before-promote", store.ModeNormal},
		{"restore-after-retire", "unavailable"},
		{"restore-after-promote", store.ModeRecovery},
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
				if mode := queryString(t, e.database+".restore", `SELECT mode FROM deployment`); mode != store.ModeRecovery {
					t.Fatalf("the staged copy is not marked: %q", mode)
				}
			}

			e.start(binaries.plain)
			if health := e.health(); health != tc.health {
				t.Fatalf("health after the crash %q, want %q", health, tc.health)
			}
			want := http.StatusServiceUnavailable
			if tc.health == store.ModeNormal {
				want = http.StatusOK
			}
			if code, _ := e.get("/a/x", "Authorization", "Bearer "+issuer.tokenFor(t, "alice")); code != want {
				t.Fatalf("data plane after the crash: %d, want %d", code, want)
			}
			if tc.point == "restore-after-promote" {
				// The lost outcome event is recorded from the database.
				restore := func(ev map[string]any) bool {
					id, _ := ev["transition"].(string)
					return ev["kind"] == "transition.committed" && ev["reason"] == "reconciled" && strings.HasPrefix(id, "operator:restore:")
				}
				events := e.eventsWhen(func(events []map[string]any) bool {
					return countKind(events, "controller.unavailable") > 0 && slices.ContainsFunc(events, restore)
				})
				if !slices.ContainsFunc(events, restore) {
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
			if health := e.health(); health != store.ModeRecovery {
				t.Fatalf("health after the repeated restore %q", health)
			}
			e.stop()
			if _, err := os.Stat(e.database + ".restore"); err == nil {
				t.Fatal("the staging file remains")
			}
		})
	}
}
