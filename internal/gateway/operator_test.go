package gateway

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestNativeOperatorArguments(t *testing.T) {
	e := newEnv(t)
	e.writeNative(e.nativeSettings())
	legacy := filepath.Join(e.dir, "caddy.json")
	if err := os.WriteFile(legacy, legacyCaddyFile(e), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		nil,
		{"unknown"},
		{"install"},
		{"install", "--config", e.config},
		{"enrol", "--config", e.config, "--admin", "root"},
		{"backup", "--config", e.config},
		{"restore", "--config", e.config},
		{"migrate", "--config", e.config, "extra"},
		{"install", "--config", e.config, "--admin", "root", "--unknown", "x"},
		{"install", "--config", e.config, "--config", e.config, "--admin", "root"},
		{"install", "--config", e.config, "--admin", "root", "--admin", "other"},
		{"install", "--config", legacy, "--admin", "root"},
	} {
		cmd := exec.Command(binaries.plain, args...)
		out, err := cmd.CombinedOutput()
		if err == nil || cmd.ProcessState.ExitCode() != 1 || len(out) == 0 {
			t.Fatalf("arguments %v: exit %v, output %s", args, err, out)
		}
		if _, err := os.Stat(e.database); !os.IsNotExist(err) {
			t.Fatalf("invalid arguments created the database: %v", args)
		}
		t.Logf("%v: exit 1, database absent", args)
	}
}

func TestNativeOperatorPaths(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	before := fileSHA256(t, e.database)
	for _, output := range []string{
		e.database, e.auditLog, e.publicKey, e.config,
		e.database + ".lock",
		filepath.Join(e.dir, "missing", "backup.db"),
	} {
		if out, code := e.runOperator(binaries.plain, nil, "backup", "--output", output); code != 1 {
			t.Fatalf("unsafe backup destination %s: exit %d: %s", output, code, out)
		}
		if fileSHA256(t, e.database) != before {
			t.Fatal("refused backup changed the live database")
		}
	}
	output := filepath.Join(e.dir, "unsafe-partial.db")
	if err := os.Link(e.database, output+".partial"); err != nil {
		t.Fatal(err)
	}
	if out, code := e.runOperator(binaries.plain, nil, "backup", "--output", output); code != 1 {
		t.Fatalf("live database substituted as a backup partial: %d %s", code, out)
	}
	if fileSHA256(t, e.database) != before {
		t.Fatal("path substitution changed live authority")
	}
	unknown := filepath.Join(e.dir, "unknown.db.partial")
	if err := os.WriteFile(unknown, []byte("unrelated file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, code := e.runOperator(binaries.plain, nil, "backup", "--output", filepath.Join(e.dir, "unknown.db")); code != 1 {
		t.Fatalf("unknown temporary file was accepted: %d %s", code, out)
	}
	if data, err := os.ReadFile(unknown); err != nil || string(data) != "unrelated file" {
		t.Fatal("unrelated temporary file was changed")
	}
}

func TestNativeOperatorOwnership(t *testing.T) {
	e := newEnv(t)
	seed(t, e.database, e.protectedPolicy(), seedOptions{})
	lock, err := lockDeployment(e.database)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	before := fileSHA256(t, e.database)
	for _, tc := range []struct {
		command string
		args    []string
	}{
		{"backup", []string{"--output", filepath.Join(e.dir, "held.db")}},
		{"migrate", nil},
		{"enrol", []string{"--admin", "root", "--permissions", "read"}},
	} {
		if out, code := e.runOperator(binaries.plain, nil, tc.command, tc.args...); code != 1 {
			t.Fatalf("operator entered an owned deployment: %d %s", code, out)
		}
	}
	if fileSHA256(t, e.database) != before {
		t.Fatal("ownership refusals changed the database")
	}
	if _, err := os.Stat(e.auditLog); !os.IsNotExist(err) {
		t.Fatal("refused operator opened a second audit writer")
	}
}
