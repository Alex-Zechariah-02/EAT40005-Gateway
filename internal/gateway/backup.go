// Operator backup command and backup manifest.

package gateway

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

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
