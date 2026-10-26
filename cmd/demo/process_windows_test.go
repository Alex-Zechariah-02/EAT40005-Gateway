package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrivateDirectoryDACL(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	if err := privateDirectory(dir); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{privateKeyFile, configFile, "state.db-wal", "state.db-shm", "backup.db", "backup.db.manifest.json", "audit.log.1"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("test-owned access probe"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("pwsh.exe", "-NoProfile", "-Command", `$ErrorActionPreference = 'Stop'; $parent = Get-Acl -LiteralPath $env:DEMO_ACL_DIR; if (!$parent.AreAccessRulesProtected) { throw 'directory inherits its parent DACL' }; @(Get-Item -LiteralPath $env:DEMO_ACL_DIR) + @(Get-ChildItem -LiteralPath $env:DEMO_ACL_DIR) | ForEach-Object { $acl = Get-Acl -LiteralPath $_.FullName; if ($acl.Sddl -match ';;;(AU|BU|WD)\)') { throw 'broad access inherited' }; [pscustomobject]@{Name = $_.Name; Sddl = $acl.Sddl} | ConvertTo-Json -Compress }`)
	cmd.Env = append(os.Environ(), "DEMO_ACL_DIR="+dir)
	output, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(output), privateKeyFile) {
		t.Fatalf("DACL inspection: %v: %s", err, output)
	}
	t.Logf("protected directory and inherited file DACLs:\n%s", output)
	t.Log("current-user creation/access verified; other-account execution unavailable; SYSTEM and administrator access deliberately retained")
}
