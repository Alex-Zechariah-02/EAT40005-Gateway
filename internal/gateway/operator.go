package gateway

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Local operator commands. They run only on the gateway host, never over
// HTTP, and act on the database and audit log named in the gateway's own
// configuration file. Each needs the gateway stopped: the database's
// exclusive lock refuses them while it runs.

const (
	enrolmentLifetime = 24 * time.Hour
	backupFormat      = "gateway-backup-1"
)

// operatorContext is the deployment an operator command acts on.
type operatorContext struct {
	config Config
	audit  *auditLog
}

// openAudit opens the deployment's audit log. Commands that act on an
// existing database call it only after that database proved not to be held
// by a running gateway, whose audit log has a single writer.
func (o *operatorContext) openAudit() {
	o.audit = openAudit(o.config.AuditLog, o.config.AuditMaxBytes, o.config.AuditMaxFiles)
}

// close writes the command's queued audit events and closes the log; an
// incomplete write is reported, the command's result is unchanged.
func (o *operatorContext) close() {
	if o.audit == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), auditDrainWait)
	defer cancel()
	if err := o.audit.close(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "gateway:", err)
	}
}

// record durably writes an operator event. No operator change proceeds
// without its record.
func (o *operatorContext) record(e Event) error {
	e.Actor = "operator"
	ctx, cancel := context.WithTimeout(context.Background(), o.config.AuditTimeout)
	defer cancel()
	if err := o.audit.Commit(ctx, e); err != nil {
		return fmt.Errorf("recording %s: %w", e.Kind, err)
	}
	return nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func runInstall(o *operatorContext, flags map[string]string) (int, error) {
	name := flags["admin"]
	if !isAdminName(name) {
		return 1, fmt.Errorf("administrator name %q is not valid", name)
	}
	o.openAudit()

	now := time.Now()
	installation := randomHex(16)
	secret := newToken()
	first := enrolment{Name: name, Secret: tokenHash(secret), Permissions: permAll, ExpiresAt: now.Add(enrolmentLifetime)}
	t := transitionRecord{ID: "operator:install:" + installation, Kind: "operator.install", Actor: "operator", Target: name, Digest: "operator"}
	if err := o.record(Event{Kind: "transition.intent", Transition: t.ID}); err != nil {
		return 1, err
	}
	st, err := createStore(o.config.Database, installation, first, t, now)
	if err != nil {
		return 1, fmt.Errorf("installing: %w", err)
	}
	defer st.close()
	if err := o.record(Event{Kind: "transition.committed", Transition: t.ID}); err != nil {
		return 1, err
	}
	if err := st.markAudited(context.Background(), t.ID, time.Now()); err != nil {
		return 1, err
	}

	fmt.Printf("installed %s (installation %s, mode %s)\n", o.config.Database, installation, modeBootstrap)
	fmt.Printf("enrolment secret for %s, valid until %s: %s\n", name, first.ExpiresAt.UTC().Format(time.RFC3339), secret)
	return 0, nil
}

func runEnrol(o *operatorContext, flags map[string]string) (int, error) {
	name := flags["admin"]
	if !isAdminName(name) {
		return 1, fmt.Errorf("administrator name %q is not valid", name)
	}
	permissions, err := parsePermissions(flags["permissions"])
	if err != nil {
		return 1, err
	}
	st, err := openStore(o.config.Database)
	if err != nil {
		return 1, err
	}
	defer st.close()
	o.openAudit()

	now := time.Now()
	secret := newToken()
	e := enrolment{Name: name, Secret: tokenHash(secret), Permissions: permissions, ExpiresAt: now.Add(enrolmentLifetime)}
	t := transitionRecord{ID: fmt.Sprintf("operator:enrol:%s:%s", name, randomHex(8)), Kind: "operator.enrol", Actor: "operator", Target: name, Digest: "operator"}
	if err := o.record(Event{Kind: "transition.intent", Transition: t.ID}); err != nil {
		return 1, err
	}
	if err := st.issueEnrolment(context.Background(), t, e, now); err != nil {
		return 1, err
	}
	if err := o.record(Event{Kind: "transition.committed", Transition: t.ID}); err != nil {
		return 1, err
	}
	if err := st.markAudited(context.Background(), t.ID, time.Now()); err != nil {
		return 1, err
	}
	fmt.Printf("enrolment secret for %s (%v), valid until %s: %s\n", name, permissions.names(), e.ExpiresAt.UTC().Format(time.RFC3339), secret)
	return 0, nil
}

func runMigrate(o *operatorContext) (int, error) {
	st, from, err := openForMigration(o.config.Database)
	if err != nil {
		return 1, err
	}
	defer st.close()
	o.openAudit()

	t := transitionRecord{
		ID:     fmt.Sprintf("operator:migrate:%d:%s", databaseVersion, randomHex(8)),
		Kind:   "operator.migrate",
		Actor:  "operator",
		Target: fmt.Sprintf("schema version %d", databaseVersion),
		Digest: "operator",
	}
	if err := o.record(Event{Kind: "transition.intent", Transition: t.ID}); err != nil {
		return 1, err
	}
	if err := st.migrate(context.Background(), from, t, time.Now()); err != nil {
		return 1, fmt.Errorf("migrating: %w", err)
	}
	if err := o.record(Event{Kind: "transition.committed", Transition: t.ID}); err != nil {
		return 1, err
	}
	if err := st.markAudited(context.Background(), t.ID, time.Now()); err != nil {
		return 1, err
	}
	fmt.Printf("migrated %s from schema version %d to %d\n", o.config.Database, from, databaseVersion)
	return 0, nil
}

// backupManifest describes a backup file. Its checksum detects a damaged or
// altered copy; it does not prove who made the backup.
type backupManifest struct {
	Format        string    `json:"format"`
	Installation  string    `json:"installation"`
	SchemaVersion int       `json:"schema_version"`
	Mode          string    `json:"mode"`
	Revision      int64     `json:"confirmed_revision"`
	CreatedAt     time.Time `json:"created_at"`
	SHA256        string    `json:"sha256"`
	Bytes         int64     `json:"bytes"`
}

func manifestPath(backup string) string { return backup + ".manifest.json" }

// runBackup writes a consistent snapshot with VACUUM INTO, which reads the
// database through SQLite and so includes changes still in the WAL. The
// snapshot is verified before it gets its final name, so an interrupted or
// failed backup never looks complete.
func runBackup(o *operatorContext, flags map[string]string) (int, error) {
	output := flags["output"]
	if output == "" {
		return 1, errors.New("--output is required")
	}
	if _, err := os.Lstat(output); !errors.Is(err, fs.ErrNotExist) {
		return 1, fmt.Errorf("%s already exists; a backup never replaces a file", output)
	}
	st, err := openStore(o.config.Database)
	if err != nil {
		return 1, err
	}
	defer st.close()
	o.openAudit()
	a, err := st.load(context.Background())
	if err != nil {
		return 1, err
	}

	partial := output + ".partial"
	if _, err := os.Lstat(manifestPath(output)); !errors.Is(err, fs.ErrNotExist) {
		return 1, errors.New("the backup manifest already exists")
	}
	if err := removeStagedDatabase(partial, a.installation); err != nil {
		return 1, err
	}
	if err := createPrivate(partial); err != nil {
		return 1, err
	}
	defer removeFiles(partial)
	if _, err := st.db.Exec("VACUUM INTO ?", partial); err != nil {
		return 1, fmt.Errorf("writing snapshot: %w", err)
	}
	crashPoint("backup-before-verify")
	if err := checkDatabaseFile(partial); err != nil {
		return 1, fmt.Errorf("verifying snapshot: %w", err)
	}
	if err := os.Link(partial, output); err != nil {
		return 1, err
	}
	complete := false
	defer func() {
		if !complete {
			removeFiles(output)
		}
	}()
	sum, size, err := fileDigest(output)
	if err != nil {
		return 1, err
	}
	manifest := backupManifest{
		Format:        backupFormat,
		Installation:  a.installation,
		SchemaVersion: databaseVersion,
		Mode:          a.mode,
		Revision:      a.confirmed,
		CreatedAt:     time.Now().UTC(),
		SHA256:        sum,
		Bytes:         size,
	}
	if err := writeNewFile(manifestPath(output), manifest); err != nil {
		return 1, err
	}
	defer func() {
		if !complete {
			removeFiles(manifestPath(output))
		}
	}()
	if err := o.record(Event{Kind: "operator.backup", File: filepath.Base(output), Revision: a.confirmed}); err != nil {
		return 1, err
	}
	complete = true
	fmt.Printf("backup %s written (revision %d, sha256 %s)\n", output, a.confirmed, sum)
	return 0, nil
}

// checkDatabaseFile opens a snapshot with the gateway's settings in rollback
// journal mode, so it is a single self-contained file, and runs the full
// integrity check and the schema checks.
func checkDatabaseFile(path string) error {
	db, err := connect(path, "DELETE")
	if err != nil {
		return err
	}
	defer db.Close()
	var check string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&check); err != nil || check != "ok" {
		return fmt.Errorf("integrity check failed: %v %s", err, check)
	}
	return verify(db)
}

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
	t := transitionRecord{ID: "operator:restore:" + episode, Kind: "operator.restore", Actor: "operator", Target: filepath.Base(backup), Digest: manifest.SHA256}
	if err := o.record(Event{Kind: "transition.intent", Transition: t.ID, File: filepath.Base(backup)}); err != nil {
		return 1, err
	}
	if err := markRecovery(staging, t, episode, now); err != nil {
		return 1, fmt.Errorf("marking the staged database: %w", err)
	}
	crashPoint("restore-before-promote")

	retired := ""
	if _, err := os.Stat(live); err == nil {
		retired = fmt.Sprintf("%s.replaced-%s-%s", live, now.UTC().Format("20060102T150405Z"), randomHex(4))
		if err := os.Rename(live, retired); err != nil {
			return 1, err
		}
	}
	crashPoint("restore-after-retire")
	if err := os.Rename(staging, live); err != nil {
		if retired != "" {
			err = errors.Join(err, os.Rename(retired, live))
		}
		return 1, err
	}
	crashPoint("restore-after-promote")

	if err := o.record(Event{Kind: "transition.committed", Transition: t.ID, File: filepath.Base(backup)}); err != nil {
		return 1, err
	}
	// The restore is complete. If its audit receipt cannot be recorded here,
	// the gateway records the outcome event again at its next startup.
	if st, err := openStore(live); err == nil {
		st.markAudited(context.Background(), t.ID, time.Now())
		st.close()
	}
	fmt.Printf("restored %s as %s in %s mode (episode %s)\n", backup, live, modeRecovery, episode)
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
	if err := decodeStrict(data, &manifest); err != nil || manifest.Format != backupFormat {
		return manifest, errors.New("the backup manifest is not a gateway backup manifest")
	}
	sum, size, err := fileDigest(backup)
	if err != nil {
		return manifest, err
	}
	if sum != manifest.SHA256 || size != manifest.Bytes {
		return manifest, errors.New("the backup does not match its manifest checksum")
	}
	if manifest.SchemaVersion != databaseVersion {
		return manifest, fmt.Errorf("the backup has schema version %d, this gateway needs %d", manifest.SchemaVersion, databaseVersion)
	}
	db, err := sql.Open("sqlite", databaseURI(backup)+"?mode=ro&_pragma=foreign_keys(ON)")
	if err != nil {
		return manifest, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var check string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&check); err != nil || check != "ok" {
		return manifest, fmt.Errorf("the backup fails the integrity check: %v %s", err, check)
	}
	if err := verify(db); err != nil {
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
		st, err := openStore(live)
		switch {
		case err == nil:
			a, loadErr := st.load(context.Background())
			closeErr := st.close()
			if loadErr == nil && a.installation != installation {
				return fmt.Errorf("the backup belongs to installation %s, the live database to %s", installation, a.installation)
			}
			if closeErr != nil {
				return closeErr
			}
		case !errors.Is(err, errUnavailable):
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
	db, err := sql.Open("sqlite", databaseURI(source)+"?mode=ro")
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err := createPrivate(target); err != nil {
		return err
	}
	if _, err := db.Exec("VACUUM INTO ?", target); err != nil {
		return err
	}
	return checkDatabaseFile(target)
}

func markRecovery(path string, t transitionRecord, episode string, now time.Time) error {
	db, err := connect(path, "DELETE")
	if err != nil {
		return err
	}
	s := &store{db: db}
	err = s.declareRecovery(context.Background(), t, episode, now)
	return errors.Join(err, s.close())
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
	db, err := sql.Open("sqlite", databaseURI(path)+"?mode=ro&_pragma=foreign_keys(ON)")
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	err = verify(db)
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

func sameFile(a, b string) bool {
	first, err1 := os.Stat(a)
	second, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(first, second)
}

func fileDigest(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	sum := sha256.New()
	size, err := io.Copy(sum, file)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(sum.Sum(nil)), size, nil
}

// writeNewFile writes value as JSON to a file that must not exist yet.
func writeNewFile(path string, value any) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		file.Close()
		os.Remove(path)
		return err
	}
	_, err = file.Write(append(data, '\n'))
	if err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		os.Remove(path)
	}
	return err
}

// executeOperator holds ownership across restore's SQLite file replacement.
func executeOperator(command string, o *operatorContext, flags map[string]string) (int, error) {
	if err := operatorPaths(flags["config"], o.config, command, flags); err != nil {
		return 1, err
	}
	lock, err := lockDeployment(o.config.Database)
	if err != nil {
		return 1, err
	}
	defer lock.Close()
	defer o.close()
	switch command {
	case "install":
		return runInstall(o, flags)
	case "enrol":
		return runEnrol(o, flags)
	case "migrate":
		return runMigrate(o)
	case "backup":
		return runBackup(o, flags)
	case "restore":
		return runRestore(o, flags)
	}
	return 1, errors.New("unknown operator command")
}
