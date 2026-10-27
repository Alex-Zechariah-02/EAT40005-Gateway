package gateway

import (
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Alex-Zechariah-02/EAT40005-Gateway/internal/demoports"
)

func demoTestAddress(t *testing.T, base string) string {
	t.Helper()
	address, err := demoports.Offset(base)
	if err != nil {
		t.Fatal(err)
	}
	return address
}

func demoCommand(t *testing.T, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(binaries.demo, args...)
	output, err := cmd.CombinedOutput()
	if err != nil && cmd.ProcessState == nil {
		t.Fatal(err)
	}
	code := cmd.ProcessState.ExitCode()
	t.Logf("demo %v: exit %d (credential output withheld)", args, code)
	return string(output), code
}

func startDemo(t *testing.T, dir string) (*exec.Cmd, io.WriteCloser, <-chan error, *logBuffer) {
	t.Helper()
	cmd := exec.Command(binaries.demo, "run", "-dir", dir, "-gateway", binaries.plain, "-fixture", binaries.fixture, "-stdin")
	output := &logBuffer{}
	cmd.Stdout, cmd.Stderr = output, output
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() {
		exited <- cmd.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		input.Close()
		select {
		case <-exited:
		case <-time.After(15 * time.Second):
			cmd.Process.Kill()
			<-exited
			t.Errorf("demo cleanup exceeded its bound: %s", output)
		}
	})
	waitFor(t, time.Now().Add(30*time.Second), "native demonstration admission", func() bool {
		return strings.Contains(output.String(), "gateway   process")
	})
	return cmd, input, exited, output
}

func TestNativeDemoCommands(t *testing.T) {
	parent := t.TempDir()
	web := filepath.Join(parent, "web")
	for _, page := range []string{"admin", "demo"} {
		dir := filepath.Join(web, page)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("owned test page"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dir := filepath.Join(parent, "demo")
	args := []string{"setup", "-dir", dir, "-web", web, "-gateway", binaries.plain, "-admin", "root"}
	output, code := demoCommand(t, args...)
	if code != 0 || enrolmentSecret(t, output) == "" {
		t.Fatalf("native demo setup exit %d", code)
	}
	if _, code := demoCommand(t, args...); code != 1 {
		t.Fatal("setup reused an existing directory")
	}
	data, err := os.ReadFile(filepath.Join(dir, "gateway.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeNativeFile(data); err != nil {
		t.Fatalf("generated configuration is not strict native JSON: %v", err)
	}
	for _, args := range [][]string{
		{"unknown"},
		{"token", "-dir", dir, "-subject", "has spaces"},
		{"token", "-dir", dir, "-subject", "staff-01", "-ttl", "0s"},
		{"token", "-dir", dir, "-subject", "staff-01", "-ttl", "2h"},
		{"reset", "-dir", parent},
		{"run", "-dir", dir},
		{"setup", "-dir", filepath.Join(parent, "failed"), "-web", web, "-gateway", filepath.Join(parent, "absent.exe")},
	} {
		if _, code := demoCommand(t, args...); code == 0 {
			t.Fatalf("invalid demo arguments accepted: %v", args)
		}
	}
	if _, err := os.Stat(filepath.Join(parent, "failed")); !os.IsNotExist(err) {
		t.Fatal("failed setup left its directory or signing key")
	}
	retained := `{"msg":"protected admission","admission":"ready","probe":"retained-test-record"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "gateway.log"), []byte(retained), 0o600); err != nil {
		t.Fatal(err)
	}
	for round := range 2 {
		_, input, exited, output := startDemo(t, dir)
		if !strings.Contains(output.String(), "protected admission: bootstrap_locked") {
			t.Fatalf("listener startup mistaken for admission readiness: %s", output)
		}
		tokenOutput, code := demoCommand(t, "token", "-dir", dir, "-subject", "staff-01")
		if code != 0 {
			t.Fatal("demo token failed")
		}
		token := strings.SplitN(tokenOutput, "\n", 2)[0]
		e := &env{t: t, dataAddr: demoTestAddress(t, "127.0.0.2:9480"), admin: demoTestAddress(t, "127.0.0.1:9440")}
		if code, _ := e.get("/orders", "Authorization", "Bearer "+token); code != http.StatusServiceUnavailable {
			t.Fatalf("bootstrap data plane returned %d", code)
		}
		if _, code := demoCommand(t, "reset", "-dir", dir); code != 0 {
			t.Fatal("identified fixture reset failed")
		}
		if output, code := demoCommand(t, "events", "-dir", dir, "-last", "2"); code != 0 || !strings.Contains(output, "rotated files were not searched") {
			t.Fatal("events command did not disclose its history boundary")
		}
		input.Close()
		select {
		case err := <-exited:
			if err != nil {
				t.Fatalf("demo graceful exit: %v: %s", err, output)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("demo did not reap its children")
		}
		for _, addr := range []string{"127.0.0.1:9440", "127.0.0.2:9480", "127.0.0.1:9501", "127.0.0.1:9502", "127.0.0.1:9503", "127.0.0.1:9601", "127.0.0.1:9602", "127.0.0.1:9603"} {
			addr = demoTestAddress(t, addr)
			if dialable(addr) {
				t.Fatalf("demo round %d leaked listener %s", round, addr)
			}
		}
		log, err := os.ReadFile(filepath.Join(dir, "gateway.log"))
		if err != nil || !strings.Contains(string(log), `"msg":"stopped"`) || !strings.Contains(string(log), retained) || strings.Contains(string(log), token) {
			t.Fatal("native gateway did not stop cleanly, or disclosed a token")
		}
	}
	// A gateway crash must stop the launcher and its three remaining children.
	_, input, exited, runOutput := startDemo(t, dir)
	for line := range strings.Lines(runOutput.String()) {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "gateway" && fields[1] == "process" {
			pid, err := strconv.Atoi(fields[2])
			if err != nil {
				t.Fatal(err)
			}
			process, err := os.FindProcess(pid)
			if err != nil {
				t.Fatal(err)
			}
			if err := process.Kill(); err != nil {
				t.Fatal(err)
			}
		}
	}
	select {
	case err := <-exited:
		if err == nil {
			t.Fatal("gateway crash was reported as a clean demonstration exit")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("gateway crash did not stop the demonstration")
	}
	input.Close()
	for _, addr := range []string{"127.0.0.1:9440", "127.0.0.2:9480", "127.0.0.1:9501", "127.0.0.1:9502", "127.0.0.1:9503", "127.0.0.1:9601", "127.0.0.1:9602", "127.0.0.1:9603"} {
		addr = demoTestAddress(t, addr)
		if dialable(addr) {
			t.Fatalf("crashed demo leaked listener %s", addr)
		}
	}
	if _, code := demoCommand(t, "run", "-dir", dir, "-gateway", filepath.Join(parent, "absent.exe"), "-fixture", binaries.fixture); code != 1 {
		t.Fatal("partial demo startup did not report a failure")
	}
	for _, addr := range []string{"127.0.0.1:9501", "127.0.0.1:9502", "127.0.0.1:9503", "127.0.0.1:9601", "127.0.0.1:9602", "127.0.0.1:9603"} {
		addr = demoTestAddress(t, addr)
		if dialable(addr) {
			t.Fatalf("partial startup leaked fixture listener %s", addr)
		}
	}
	before := fileSHA256(t, filepath.Join(dir, "state.db"))
	if err := os.WriteFile(filepath.Join(dir, "caddy.json"), []byte(`{"apps":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, code := demoCommand(t, "reset", "-dir", dir); code != 1 {
		t.Fatal("stale Caddy demo was accepted")
	}
	if fileSHA256(t, filepath.Join(dir, "state.db")) != before {
		t.Fatal("rejected demo operation changed SQLite state")
	}
}
