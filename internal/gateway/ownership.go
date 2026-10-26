package gateway

import (
	"fmt"
	"os"
)

// lockDeployment uses a stable companion file: SQLite's own exclusive lock
// cannot protect a pathname while restore replaces the underlying database.
// The file is never unlinked, and the OS releases ownership after a crash.
func lockDeployment(database string) (*os.File, error) {
	path := database + ".lock"
	if err := regularPath(path); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockFile(file); err != nil {
		file.Close()
		return nil, fmt.Errorf("database %q is held by another gateway process: %w", database, err)
	}
	return file, nil
}
