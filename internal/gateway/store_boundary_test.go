package gateway

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEnrolmentExpiryAtConsumption(t *testing.T) {
	for _, delta := range []time.Duration{-time.Millisecond, 0, time.Millisecond} {
		t.Run(delta.String(), func(t *testing.T) {
			e := newEnv(t)
			seed(t, e.database, e.protectedPolicy(), seedOptions{})
			st, err := openStore(e.database)
			if err != nil {
				t.Fatal(err)
			}
			defer st.close()
			issued := time.Now().UTC().Truncate(time.Millisecond)
			expires := issued.Add(time.Minute)
			pending := enrolment{Name: "late", Secret: tokenHash(newToken()), Permissions: permRead, ExpiresAt: expires}
			issue := transitionRecord{ID: "operator:expiry-test", Kind: "operator.enrol", Actor: "operator", Target: pending.Name, Digest: "test"}
			if err := st.issueEnrolment(t.Context(), issue, pending, issued); err != nil {
				t.Fatal(err)
			}
			// The HTTP handler can read a valid secret before password hashing or
			// waiting for the controller. Consumption must check its lifetime too.
			observed, found, err := st.enrolment(t.Context(), pending.Secret)
			if err != nil || !found {
				t.Fatalf("pending enrolment: found=%t err=%v", found, err)
			}
			consume := transitionRecord{ID: "enrol:expiry-test", Kind: "admin.enrol", Actor: pending.Name, Target: pending.Name, Digest: "test"}
			err = st.commitEnrolment(t.Context(), consume, observed, harnessHash(), expires.Add(delta))
			if delta < 0 {
				if err != nil || count(t, st.db, `SELECT COUNT(*) FROM administrator WHERE name = 'late'`) != 1 {
					t.Fatalf("valid enrolment was not consumed: %v", err)
				}
				if err := st.commitEnrolment(t.Context(), consume, observed, harnessHash(), expires.Add(delta)); !errors.Is(err, errNotUpdated) {
					t.Fatalf("consumed enrolment replay: %v", err)
				}
				return
			}
			if !errors.Is(err, errNotUpdated) {
				t.Errorf("expired enrolment was consumed: %v", err)
			}
			if count(t, st.db, `SELECT COUNT(*) FROM administrator WHERE name = 'late'`) != 0 ||
				count(t, st.db, `SELECT COUNT(*) FROM enrolment WHERE administrator = 'late'`) != 1 ||
				count(t, st.db, `SELECT COUNT(*) FROM transition WHERE id = ?`, consume.ID) != 0 {
				t.Error("expired enrolment changed credential, pending secret or history")
			}
		})
	}
}

func TestOperatorLiteralDatabaseAndBackupPaths(t *testing.T) {
	e := newEnv(t)
	e.database = filepath.Join(e.dir, "state%20#literal.db")
	e.writeConfig(configOptions{})
	out, code := e.runOperator(binaries.plain, nil, "install", "--admin", "root")
	if code != 0 {
		t.Fatalf("install at a literal path: %d %s", code, out)
	}
	if info, err := os.Stat(e.database); err != nil || info.Size() == 0 {
		t.Fatalf("the configured database file was not populated: %v", err)
	}
	e.start(binaries.plain)
	if code, body := e.enrol("root", enrolmentSecret(t, out), testPassword()); code != http.StatusCreated {
		t.Fatalf("enrol at a literal path: %d %s", code, body)
	}
	e.stop()
	backup := filepath.Join(e.dir, "backup%20#literal.db")
	if out, code := e.runOperator(binaries.plain, nil, "backup", "--output", backup); code != 0 {
		t.Fatalf("backup at a literal path: %d %s", code, out)
	}
	if out, code := e.runOperator(binaries.plain, nil, "restore", "--backup", backup); code != 0 {
		t.Fatalf("restore at a literal path: %d %s", code, out)
	}
	e.start(binaries.plain)
	if mode := e.health(); mode != modeRecovery {
		t.Fatalf("restored mode %q, want recovery_required", mode)
	}
}
