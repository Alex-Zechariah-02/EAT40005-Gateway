// Path checks for operator commands.

package gateway

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
)

func pathsAlias(a, b string) bool {
	a, _ = filepath.Abs(a)
	b, _ = filepath.Abs(b)
	if runtime.GOOS == "windows" && strings.EqualFold(a, b) || a == b {
		return true
	}
	return sameFile(a, b)
}

func operatorPaths(configPath string, cfg Config, command string, flags map[string]string) error {
	protected := []struct{ name, path string }{
		{"configuration", configPath},
		{"database", cfg.Database},
		{"audit log", cfg.AuditLog},
		{"deployment lock", cfg.Database + ".lock"},
		{"database WAL", cfg.Database + "-wal"},
		{"database SHM", cfg.Database + "-shm"},
		{"database journal", cfg.Database + "-journal"},
	}
	for _, key := range cfg.JWT.Keys {
		protected = append(protected, struct{ name, path string }{"JWT public key", key.PublicKeyFile})
	}
	for i, file := range protected {
		if err := regularPath(file.path); err != nil {
			return fmt.Errorf("%s: %w", file.name, err)
		}
		for _, previous := range protected[:i] {
			if pathsAlias(file.path, previous.path) {
				return errors.New("configuration paths must name distinct files")
			}
		}
	}
	var paths []string
	switch command {
	case "backup":
		paths = []string{flags["output"], flags["output"] + ".partial", manifestPath(flags["output"])}
	case "restore":
		if pathsAlias(flags["backup"], cfg.Database) {
			return errors.New("the backup cannot be the live database")
		}
		paths = []string{flags["backup"], manifestPath(flags["backup"]), cfg.Database + ".restore", cfg.Database + ".restore-wal", cfg.Database + ".restore-shm", cfg.Database + ".restore-journal"}
	}
	for i, path := range paths {
		if err := regularPath(path); err != nil {
			return err
		}
		for _, other := range protected {
			if pathsAlias(path, other.path) {
				return errors.New("operator paths conflict with deployment authority")
			}
		}
		for _, other := range paths[:i] {
			if pathsAlias(path, other) {
				return errors.New("operator paths conflict with deployment authority or another operation file")
			}
		}
	}
	return nil
}
