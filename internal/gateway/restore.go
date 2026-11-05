// Operator restore command and its checks.

package gateway

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/audit"
	"github.com/Alex-Zechariah-02/apiward/internal/fault"
	"github.com/Alex-Zechariah-02/apiward/internal/policy"
	"github.com/Alex-Zechariah-02/apiward/internal/store"
)

// runRestore performs a declared historical restore. The backup is copied
// through SQLite into a staging file next to the live database, checked, and
// marked recovery_required there, with all restored authority quarantined,
// before it can become live. Each interruption leaves the original database,
// no database (the gateway stays unavailable) or the marked copy, never an
// unmarked historical database.
func runRestore(o *operatorContext, flags map[string]string) (int, error) {
	backup := flags["backup"]
	live := o.config.Database
	if backup == "" {
		return 1, errors.New("--backup is required")
	}
	if sameFile(backup, live) {
		return 1, errors.New("the backup cannot be the live database")
	}
	manifest, err := checkBackup(backup)
	if err != nil {
		return 1, err
	}
	if err := checkLive(live, manifest.Installation); err != nil {
		return 1, err
	}
	o.openAudit()

	staging := live + ".restore"
	if err := removeStagedDatabase(staging, manifest.Installation); err != nil {
		return 1, err
	}
	defer removeFiles(staging, staging+"-wal", staging+"-shm", staging+"-journal")
	if err := copyDatabase(backup, staging); err != nil {
		return 1, fmt.Errorf("staging the backup: %w", err)
	}

	now := time.Now()
	episode := randomHex(16)
	t := store.TransitionRecord{ID: "operator:restore:" + episode, Kind: "operator.restore", Actor: "operator", Target: filepath.Base(backup), Digest: manifest.SHA256}
	if err := o.record(audit.Event{Kind: "transition.intent", Transition: t.ID, File: filepath.Base(backup)}); err != nil {
		return 1, err
	}
	if err := markRecovery(staging, t, episode, now); err != nil {
		return 1, fmt.Errorf("marking the staged database: %w", err)
	}
	fault.CrashPoint("restore-before-promote")

	retired := ""
	if _, err := os.Stat(live); err == nil {
		retired = fmt.Sprintf("%s.replaced-%s-%s", live, now.UTC().Format("20060102T150405Z"), randomHex(4))
		if err := os.Rename(live, retired); err != nil {
			return 1, err
		}
	}
	fault.CrashPoint("restore-after-retire")
	if err := os.Rename(staging, live); err != nil {
		if retired != "" {
			err = errors.Join(err, os.Rename(retired, live))
		}
		return 1, err
	}
	fault.CrashPoint("restore-after-promote")

	if err := o.record(audit.Event{Kind: "transition.committed", Transition: t.ID, File: filepath.Base(backup)}); err != nil {
		return 1, err
	}
	// The restore is complete. If its audit receipt cannot be recorded here,
	// the gateway records the outcome event again at its next startup.
	if st, err := store.OpenStore(live); err == nil {
		st.MarkAudited(context.Background(), t.ID, time.Now())
		st.Close()
	}
	fmt.Printf("restored %s as %s in %s mode (episode %s)\n", backup, live, store.ModeRecovery, episode)
	if retired != "" {
		fmt.Printf("the replaced database is kept as %s\n", retired)
	}
	fmt.Println("next: enrol an administrator, then confirm a deny-all recovery base")
	return 0, nil
}

// checkBackup verifies the backup against its manifest and runs the database
// checks on it without changing it.
func checkBackup(backup string) (backupManifest, error) {
	var manifest backupManifest
	data, err := os.ReadFile(manifestPath(backup))
	if err != nil {
		return manifest, fmt.Errorf("reading the backup manifest: %w", err)
	}
	if err := policy.DecodeStrict(data, &manifest); err != nil || manifest.Format != backupFormat {
		return manifest, errors.New("the backup manifest is not a gateway backup manifest")
	}
	sum, size, err := fileDigest(backup)
	if err != nil {
		return manifest, err
	}
	if sum != manifest.SHA256 || size != manifest.Bytes {
		return manifest, errors.New("the backup does not match its manifest checksum")
	}
	if manifest.SchemaVersion != store.DatabaseVersion {
		return manifest, fmt.Errorf("the backup has schema version %d, this gateway needs %d", manifest.SchemaVersion, store.DatabaseVersion)
	}
	db, err := sql.Open("sqlite", store.DatabaseURI(backup)+"?mode=ro&_pragma=foreign_keys(ON)")
	if err != nil {
		return manifest, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var check string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&check); err != nil || check != "ok" {
		return manifest, fmt.Errorf("the backup fails the integrity check: %v %s", err, check)
	}
	if err := store.Verify(db); err != nil {
		return manifest, fmt.Errorf("the backup is not usable: %w", err)
	}
	var installation, mode string
	var revision int64
	if err := db.QueryRow("SELECT d.installation, d.mode, coalesce(b.confirmed, 0) FROM deployment d, base b WHERE d.id = 1 AND b.id = 1").Scan(&installation, &mode, &revision); err != nil {
		return manifest, err
	}
	if installation != manifest.Installation || mode != manifest.Mode || revision != manifest.Revision {
		return manifest, errors.New("the backup authority does not match its manifest")
	}
	return manifest, nil
}

// checkLive refuses to replace a live database that is in use, that belongs to
// another installation, or that still has write-ahead log files beside it,
// which would be applied to the restored file.
func checkLive(live, installation string) error {
	if _, err := os.Stat(live); err == nil {
		st, err := store.OpenStore(live)
		switch {
		case err == nil:
			a, loadErr := st.Load(context.Background())
			closeErr := st.Close()
			if loadErr == nil && a.Installation != installation {
				return fmt.Errorf("the backup belongs to installation %s, the live database to %s", installation, a.Installation)
			}
			if closeErr != nil {
				return closeErr
			}
		case !errors.Is(err, store.ErrUnavailable):
			return err
		}
	}
	for _, companion := range []string{live + "-wal", live + "-shm"} {
		if _, err := os.Lstat(companion); !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%s exists; move the live database and its companion files aside before restoring", companion)
		}
	}
	return nil
}

// copyDatabase copies a database file through SQLite into a new file.
func copyDatabase(source, target string) error {
	db, err := sql.Open("sqlite", store.DatabaseURI(source)+"?mode=ro")
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err := store.CreatePrivate(target); err != nil {
		return err
	}
	if _, err := db.Exec("VACUUM INTO ?", target); err != nil {
		return err
	}
	return checkDatabaseFile(target)
}

func markRecovery(path string, t store.TransitionRecord, episode string, now time.Time) error {
	db, err := store.Connect(path, "DELETE")
	if err != nil {
		return err
	}
	s := store.Adopt(db)
	err = s.DeclareRecovery(context.Background(), t, episode, now)
	return errors.Join(err, s.Close())
}

func removeFiles(paths ...string) error {
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// A retry removes only a verified staging database of this installation.
// Unknown or damaged files need explicit operator disposition, not deletion.
func removeStagedDatabase(path, installation string) error {
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		for _, companion := range []string{path + "-wal", path + "-shm", path + "-journal"} {
			if _, err := os.Lstat(companion); !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("unverified staging companion %s; move it aside explicitly", companion)
			}
		}
		return nil
	} else if err != nil {
		return err
	}
	db, err := sql.Open("sqlite", store.DatabaseURI(path)+"?mode=ro&_pragma=foreign_keys(ON)")
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	err = store.Verify(db)
	var identity string
	if err == nil {
		err = db.QueryRow("SELECT installation FROM deployment WHERE id = 1").Scan(&identity)
	}
	err = errors.Join(err, db.Close())
	if err != nil || identity != installation {
		return fmt.Errorf("refusing to remove unverified staging file %s; move it aside explicitly: %w", path, errors.Join(err, errors.New("staging identity must match this installation")))
	}
	return removeFiles(path, path+"-wal", path+"-shm", path+"-journal")
}
