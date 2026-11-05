// Operator commands: install, enrol, migrate and dispatch.

package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/store"
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
	if !store.IsAdminName(name) {
		return 1, fmt.Errorf("administrator name %q is not valid", name)
	}
	o.openAudit()

	now := time.Now()
	installation := randomHex(16)
	secret := newToken()
	first := store.Enrolment{Name: name, Secret: tokenHash(secret), Permissions: store.PermAll, ExpiresAt: now.Add(enrolmentLifetime)}
	t := store.TransitionRecord{ID: "operator:install:" + installation, Kind: "operator.install", Actor: "operator", Target: name, Digest: "operator"}
	if err := o.record(Event{Kind: "transition.intent", Transition: t.ID}); err != nil {
		return 1, err
	}
	st, err := store.CreateStore(o.config.Database, installation, first, t, now)
	if err != nil {
		return 1, fmt.Errorf("installing: %w", err)
	}
	defer st.Close()
	if err := o.record(Event{Kind: "transition.committed", Transition: t.ID}); err != nil {
		return 1, err
	}
	if err := st.MarkAudited(context.Background(), t.ID, time.Now()); err != nil {
		return 1, err
	}

	fmt.Printf("installed %s (installation %s, mode %s)\n", o.config.Database, installation, store.ModeBootstrap)
	fmt.Printf("enrolment secret for %s, valid until %s: %s\n", name, first.ExpiresAt.UTC().Format(time.RFC3339), secret)
	return 0, nil
}

func runEnrol(o *operatorContext, flags map[string]string) (int, error) {
	name := flags["admin"]
	if !store.IsAdminName(name) {
		return 1, fmt.Errorf("administrator name %q is not valid", name)
	}
	permissions, err := store.ParsePermissions(flags["permissions"])
	if err != nil {
		return 1, err
	}
	st, err := store.OpenStore(o.config.Database)
	if err != nil {
		return 1, err
	}
	defer st.Close()
	o.openAudit()

	now := time.Now()
	secret := newToken()
	e := store.Enrolment{Name: name, Secret: tokenHash(secret), Permissions: permissions, ExpiresAt: now.Add(enrolmentLifetime)}
	t := store.TransitionRecord{ID: fmt.Sprintf("operator:enrol:%s:%s", name, randomHex(8)), Kind: "operator.enrol", Actor: "operator", Target: name, Digest: "operator"}
	if err := o.record(Event{Kind: "transition.intent", Transition: t.ID}); err != nil {
		return 1, err
	}
	if err := st.IssueEnrolment(context.Background(), t, e, now); err != nil {
		return 1, err
	}
	if err := o.record(Event{Kind: "transition.committed", Transition: t.ID}); err != nil {
		return 1, err
	}
	if err := st.MarkAudited(context.Background(), t.ID, time.Now()); err != nil {
		return 1, err
	}
	fmt.Printf("enrolment secret for %s (%v), valid until %s: %s\n", name, permissions.Names(), e.ExpiresAt.UTC().Format(time.RFC3339), secret)
	return 0, nil
}

func runMigrate(o *operatorContext) (int, error) {
	st, from, err := store.OpenForMigration(o.config.Database)
	if err != nil {
		return 1, err
	}
	defer st.Close()
	o.openAudit()

	t := store.TransitionRecord{
		ID:     fmt.Sprintf("operator:migrate:%d:%s", store.DatabaseVersion, randomHex(8)),
		Kind:   "operator.migrate",
		Actor:  "operator",
		Target: fmt.Sprintf("schema version %d", store.DatabaseVersion),
		Digest: "operator",
	}
	if err := o.record(Event{Kind: "transition.intent", Transition: t.ID}); err != nil {
		return 1, err
	}
	if err := st.Migrate(context.Background(), from, t, time.Now()); err != nil {
		return 1, fmt.Errorf("migrating: %w", err)
	}
	if err := o.record(Event{Kind: "transition.committed", Transition: t.ID}); err != nil {
		return 1, err
	}
	if err := st.MarkAudited(context.Background(), t.ID, time.Now()); err != nil {
		return 1, err
	}
	fmt.Printf("migrated %s from schema version %d to %d\n", o.config.Database, from, store.DatabaseVersion)
	return 0, nil
}

// executeOperator holds ownership across restore's SQLite file replacement.
func executeOperator(command string, o *operatorContext, flags map[string]string) (int, error) {
	if err := operatorPaths(flags["config"], o.config, command, flags); err != nil {
		return 1, err
	}
	lock, err := store.LockDeployment(o.config.Database)
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
