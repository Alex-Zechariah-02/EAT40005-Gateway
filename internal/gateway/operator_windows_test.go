package gateway

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Inspect actual DACLs, including sidecars, rather than interpreting Unix
// permission bits as Windows access control. This is current-user evidence;
// it does not impersonate another account or claim cross-account isolation.
func TestNativeOperatorWindowsAccess(t *testing.T) {
	e := newEnv(t)
	if _, code := e.runOperator(binaries.plain, nil, "install", "--admin", "root"); code != 0 {
		t.Fatalf("install exit %d", code)
	}
	backup := filepath.Join(e.dir, "backup.db")
	if out, code := e.runOperator(binaries.plain, nil, "backup", "--output", backup); code != 0 {
		t.Fatalf("backup exit %d: %s", code, out)
	}
	st, err := openStore(e.database)
	if err != nil {
		t.Fatal(err)
	}
	defer st.close()
	paths := []string{e.dir, e.config, e.publicKey, e.database, e.database + "-wal", e.database + "-shm", e.auditLog, backup, manifestPath(backup)}
	for _, path := range paths {
		if _, err := os.Stat(path); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("pwsh.exe", "-NoProfile", "-Command", `$ErrorActionPreference = 'Stop'; @(Get-Item -LiteralPath $env:GATEWAY_ACL_DIR) + @(Get-ChildItem -Force -LiteralPath $env:GATEWAY_ACL_DIR) | ForEach-Object { $acl = Get-Acl -LiteralPath $_.FullName; [pscustomobject]@{Name = $_.Name; Sddl = $acl.Sddl; Protected = $acl.AreAccessRulesProtected} | ConvertTo-Json -Compress }`)
	cmd.Env = append(os.Environ(), "GATEWAY_ACL_DIR="+e.dir)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("DACL inspection: %v: %s", err, output)
	}
	if len(output) == 0 {
		t.Fatal("DACL inspection returned no descriptors")
	}
	t.Logf("DACLs on actual operator artifacts:\n%s", output)
	t.Log("current user created, read and wrote this deployment; other-user access is NOT TESTED; administrative access is not excluded")
	before := fileSHA256(t, e.database)
	cmd = exec.Command("pwsh.exe", "-NoProfile", "-Command", `$ErrorActionPreference = 'Stop'; $path = Join-Path $env:GATEWAY_ACL_DIR 'gateway.json'; $saved = Get-Acl -LiteralPath $path; $denied = Get-Acl -LiteralPath $path; $sid = [System.Security.Principal.WindowsIdentity]::GetCurrent().User; $rule = [System.Security.AccessControl.FileSystemAccessRule]::new($sid, [System.Security.AccessControl.FileSystemRights]::Read, [System.Security.AccessControl.AccessControlType]::Deny); $denied.AddAccessRule($rule); try { Set-Acl -LiteralPath $path -AclObject $denied; & $env:GATEWAY_ACL_BINARY backup --config $path --output (Join-Path $env:GATEWAY_ACL_DIR 'denied.db'); if ($LASTEXITCODE -ne 1) { throw "expected permission refusal exit 1, got $LASTEXITCODE" }; 'access-denied native backup: exit 1' } finally { Set-Acl -LiteralPath $path -AclObject $saved }`)
	cmd.Env = append(os.Environ(), "GATEWAY_ACL_DIR="+e.dir, "GATEWAY_ACL_BINARY="+binaries.plain)
	output, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("test-owned access denial: %v: %s", err, output)
	}
	if fileSHA256(t, e.database) != before {
		t.Fatal("permission refusal changed live authority")
	}
	t.Logf("current-user access-denial probe (original DACL restored): %s", output)
}
