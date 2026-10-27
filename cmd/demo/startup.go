package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// A directory left by the pre-Fetch install is not necessarily a completed
// deployment. Only an identified, unused bootstrap can be preserved and set up
// afresh automatically. Policies, accounts and lifecycle state are never migrated.
// Keep the gateway's companion lock held until the caller finishes with it.
func lockUnfinishedEarlierDemo(dir string) (*os.File, error) {
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("earlier setup must be a regular directory")
	}
	if err := checkEarlierDemoForFresh(dir); err != nil {
		return nil, err
	}
	for _, name := range []string{databaseFile, databaseFile + ".lock"} {
		info, err := os.Lstat(filepath.Join(dir, name))
		if err != nil || !info.Mode().IsRegular() {
			return nil, errors.New("earlier setup requires a regular database and deployment lock")
		}
	}
	var config struct {
		Gateway struct {
			Database string `json:"database"`
		} `json:"gateway"`
	}
	data, err := os.ReadFile(filepath.Join(dir, configFile))
	if err != nil {
		return nil, err
	}
	if json.Unmarshal(data, &config) != nil || filepath.Clean(config.Gateway.Database) != filepath.Join(dir, databaseFile) {
		return nil, errors.New("earlier setup database is outside its identified state directory")
	}
	lock, err := lockEarlierDemoFile(filepath.Join(dir, databaseFile+".lock"))
	if err != nil {
		return nil, fmt.Errorf("earlier setup is held by another process: %w", err)
	}
	checked := false
	defer func() {
		if !checked {
			lock.Close()
		}
	}()
	if err := checkUnusedBootstrapDatabase(dir); err != nil {
		return nil, err
	}
	checked = true
	return lock, nil
}

func checkUnusedBootstrapDatabase(dir string) error {
	path := filepath.ToSlash(filepath.Join(dir, databaseFile))
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	location := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&_pragma=query_only(ON)"}
	db, err := sql.Open("sqlite", location.String())
	if err != nil {
		return err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&integrity); err != nil || integrity != "ok" {
		return fmt.Errorf("earlier setup database integrity check failed: %s (%v)", integrity, err)
	}
	var unused bool
	err = db.QueryRowContext(ctx, `
		SELECT d.mode = 'bootstrap_locked'
			AND b.confirmed IS NULL AND b.provisional IS NULL AND b.deadline IS NULL
			AND NOT EXISTS (SELECT 1 FROM administrator)
			AND NOT EXISTS (SELECT 1 FROM revision)
			AND NOT EXISTS (SELECT 1 FROM candidate)
			AND NOT EXISTS (SELECT 1 FROM activation)
			AND NOT EXISTS (SELECT 1 FROM override)
			AND NOT EXISTS (SELECT 1 FROM pragma_foreign_key_check)
		FROM deployment d JOIN base b ON b.id = d.id WHERE d.id = 1
	`).Scan(&unused)
	if err != nil {
		return fmt.Errorf("checking earlier setup without changing it: %w", err)
	}
	if !unused {
		return errors.New("earlier deployment has saved authority; refusing automatic replacement")
	}
	return nil
}

func preserveUnfinishedEarlierDemo(dir string) (string, error) {
	// Recheck after building: another operator may have completed the setup.
	lock, err := lockUnfinishedEarlierDemo(dir)
	if err != nil {
		return "", err
	}
	// Windows cannot rename a directory containing an open handle. Reacquire
	// the same companion lock at the preserved path and recheck the database
	// immediately afterwards. If another operator won that gap, restore the
	// original directory and refuse setup rather than replace their authority.
	if err := lock.Close(); err != nil {
		return "", err
	}
	backup := filepath.Join(filepath.Dir(dir), "state-before-fetch-"+newKey())
	if err := privateDirectory(backup); err != nil {
		return "", err
	}
	preserved := filepath.Join(backup, "state")
	if err := os.Rename(dir, preserved); err != nil {
		os.Remove(backup) // Only the empty directory this call just created.
		return "", err
	}
	lock, err = lockEarlierDemoFile(filepath.Join(preserved, databaseFile+".lock"))
	if err == nil {
		err = checkUnusedBootstrapDatabase(preserved)
		lock.Close()
	}
	if err != nil {
		if _, originalErr := os.Lstat(dir); !errors.Is(originalErr, os.ErrNotExist) {
			return "", fmt.Errorf("earlier setup remains at %s; original path is no longer empty: %w", preserved, err)
		}
		if restoreErr := os.Rename(preserved, dir); restoreErr != nil {
			return "", fmt.Errorf("earlier setup remains at %s: %w", preserved, errors.Join(err, restoreErr))
		}
		os.Remove(backup)
		return "", err
	}
	return preserved, nil
}
