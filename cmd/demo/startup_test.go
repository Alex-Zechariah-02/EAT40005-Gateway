package main

import (
	"bufio"
	"bytes"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func earlierStartupState(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	state := filepath.Join(home, "state")
	if err := os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{
		configFile:             []byte(`{"version":1,"gateway":{"database":` + string(marshal(filepath.Join(state, databaseFile))) + `}}`),
		"fixture.secret":       []byte("synthetic earlier service secret"),
		databaseFile + ".lock": nil,
	} {
		if err := os.WriteFile(filepath.Join(state, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	db, err := sql.Open("sqlite", filepath.Join(state, databaseFile))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		CREATE TABLE deployment (id INTEGER, mode TEXT);
		INSERT INTO deployment VALUES (1, 'bootstrap_locked');
		CREATE TABLE base (id INTEGER, confirmed INTEGER, provisional INTEGER, deadline INTEGER);
		INSERT INTO base VALUES (1, NULL, NULL, NULL);
		CREATE TABLE administrator (name TEXT);
		CREATE TABLE revision (id INTEGER);
		CREATE TABLE candidate (id INTEGER);
		CREATE TABLE activation (id INTEGER);
		CREATE TABLE override (id INTEGER);
		PRAGMA user_version = 4;
	`)
	closeErr := db.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("earlier state: %v %v", err, closeErr)
	}
	config, _ := demoDigest(filepath.Join(state, configFile))
	secret, _ := demoDigest(filepath.Join(state, "fixture.secret"))
	identity := map[string]string{"format": "gateway-demo-1", "directory": state, "config_sha256": config, "fixture_secret_sha256": secret}
	if err := os.WriteFile(filepath.Join(state, identityFile), indented(identity), 0600); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestUpRecognizesUnfinishedEarlierSetupBeforeBuilding(t *testing.T) {
	// This test never starts a server; use OS-allocated probe ports so the
	// package can run alongside the gateway's real-stack tests.
	addresses := []*string{&adminListen, &dataListen, &metricsListen}
	for i := range fetchServices {
		addresses = append(addresses, &fetchServices[i].listen, &fetchServices[i].control)
	}
	for _, address := range addresses {
		previous := *address
		*address = "127.0.0.1:0"
		t.Cleanup(func() { *address = previous })
	}
	home := earlierStartupState(t)
	state := filepath.Join(home, "state")
	before, err := os.ReadFile(filepath.Join(state, identityFile))
	if err != nil {
		t.Fatal(err)
	}
	// No application source is needed: an unfinished setup must ask for its
	// initial password before any build or state change. EOF cancels it.
	repository := t.TempDir()
	if err := os.WriteFile(filepath.Join(repository, "go.mod"), []byte("module startup-test\n\ngo 1.26.0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repository, "cmd", "gateway"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repository)
	previousInput := stdinLines
	stdinLines = bufio.NewReader(strings.NewReader(""))
	t.Cleanup(func() { stdinLines = previousInput })
	if err := up([]string{"-home", home}); !errors.Is(err, io.EOF) {
		t.Fatalf("unfinished setup did not ask for its initial password before building: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(state, identityFile))
	if err != nil || string(after) != string(before) {
		t.Fatal("cancelled setup changed the earlier identity")
	}
	if entries, err := os.ReadDir(home); err != nil || len(entries) != 1 {
		t.Fatalf("cancelled setup created build or backup files: %v %v", entries, err)
	}
}

func TestUnfinishedEarlierSetupIsPreservedCompletely(t *testing.T) {
	home := earlierStartupState(t)
	state := filepath.Join(home, "state")
	if err := os.WriteFile(filepath.Join(state, "operator-notes.txt"), []byte("keep this file"), 0600); err != nil {
		t.Fatal(err)
	}
	before := map[string][]byte{}
	entries, err := os.ReadDir(state)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		before[entry.Name()], err = os.ReadFile(filepath.Join(state, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
	}
	preserved, err := preserveUnfinishedEarlierDemo(state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(state); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("original path was not made available for setup: %v", err)
	}
	for name, original := range before {
		data, err := os.ReadFile(filepath.Join(preserved, name))
		if err != nil || !bytes.Equal(data, original) {
			t.Errorf("preserved file %s changed: %v", name, err)
		}
	}
}

func TestEarlierSetupWithAuthorityOrCorruptionIsNeverReplaced(t *testing.T) {
	for _, statement := range []string{
		"INSERT INTO administrator VALUES ('root')",
		"INSERT INTO revision VALUES (1)",
		"INSERT INTO candidate VALUES (1)",
		"INSERT INTO activation VALUES (1)",
		"INSERT INTO override VALUES (1)",
		"UPDATE deployment SET mode = 'normal'",
		"UPDATE base SET confirmed = 1",
		"UPDATE base SET provisional = 1",
		"UPDATE base SET deadline = 1",
		"corrupt database",
	} {
		t.Run(statement, func(t *testing.T) {
			home := earlierStartupState(t)
			state := filepath.Join(home, "state")
			path := filepath.Join(state, databaseFile)
			if statement == "corrupt database" {
				if err := os.WriteFile(path, []byte("not SQLite"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				db, err := sql.Open("sqlite", path)
				if err != nil {
					t.Fatal(err)
				}
				_, err = db.Exec(statement)
				db.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			before, _ := demoDigest(path)
			if _, err := preserveUnfinishedEarlierDemo(state); err == nil {
				t.Fatal("replaced an earlier deployment with saved authority or invalid state")
			}
			if after, err := demoDigest(path); err != nil || before != after {
				t.Fatal("refused operation changed the earlier database")
			}
			if entries, err := os.ReadDir(home); err != nil || len(entries) != 1 {
				t.Fatalf("refused operation created a backup: %v %v", entries, err)
			}
		})
	}
}

func TestEarlierSetupLockAndBindingRemainRequired(t *testing.T) {
	home := earlierStartupState(t)
	state := filepath.Join(home, "state")
	lock, err := lockEarlierDemoFile(filepath.Join(state, databaseFile+".lock"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := preserveUnfinishedEarlierDemo(state); err == nil {
		t.Error("preserved an earlier setup held by another operator")
	}
	lock.Close()
	if err := os.WriteFile(filepath.Join(state, "fixture.secret"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := preserveUnfinishedEarlierDemo(state); err == nil {
		t.Fatal("preserved an earlier setup whose identity binding changed")
	}
}
