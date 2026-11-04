// Integration harness: test binaries, environment and process control.

package gateway

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Integration tests run the real gateway binary as a separate process, so a
// crash can be injected by terminating it and the database is exercised
// across real restarts. The binaries include: the gateway and the fixture
// APIs, each also with fault-injection points compiled in, and the demo tool.
var binaries struct {
	plain, fault, fixture, fixtureFault, demo, services, accounts string
	race                                                          bool
}

// issuer signs the tokens of every test. Its public key is the trust anchor
// of every test gateway unless a test configures another.
var issuer = newTestIssuer(2048)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "gateway-bin")
	if err != nil {
		panic(err)
	}
	binaries.plain = filepath.Join(dir, "gateway.exe")
	binaries.fault = filepath.Join(dir, "gateway-fault.exe")
	binaries.fixture = filepath.Join(dir, "fixture.exe")
	binaries.fixtureFault = filepath.Join(dir, "fixture-fault.exe")
	binaries.demo = filepath.Join(dir, "demo.exe")
	binaries.services = filepath.Join(dir, "services.exe")
	binaries.accounts = filepath.Join(dir, "accounts.exe")
	// GATEWAY_TEST_RACE builds the gateway and fixture processes with the
	// race detector, which requires cgo and a C toolchain.
	var race []string
	if os.Getenv("GATEWAY_TEST_RACE") != "" {
		race = []string{"-race"}
		binaries.race = true
	}
	builds := []struct{ out, tags, pkg string }{
		{binaries.plain, "", "../../cmd/gateway"},
		{binaries.fault, "faultinject", "../../cmd/gateway"},
		{binaries.fixture, "", "../../cmd/testfixture"},
		{binaries.fixtureFault, "faultinject", "../../cmd/testfixture"},
		{binaries.demo, "", "../../cmd/demo"},
		{binaries.services, "", "../../cmd/services"},
		{binaries.accounts, "", "../../cmd/accounts"},
	}
	for _, build := range builds {
		args := append([]string{"build", "-tags", build.tags, "-o", build.out}, race...)
		cmd := exec.Command("go", append(args, build.pkg)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "building %s: %v\n%s", build.pkg, err, out)
			os.RemoveAll(dir)
			os.Exit(1)
		}
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// env is one disposable gateway deployment.
type env struct {
	t         *testing.T
	dir       string
	database  string
	auditLog  string
	publicKey string // the issuer's verification key file
	dataAddr  string
	admin     string
	metrics   string
	config    string // the gateway's configuration file
	process   *exec.Cmd
	exited    chan error
	output    *logBuffer // the process's output, readable while it runs
	a, b, c   *receiver
	session   adminSession // the harness administrator's session, if any
	sessionMu sync.Mutex   // guards session: tests call the Admin API from several goroutines
	// interruptible starts the gateway in its own process group, so that
	// interrupt can reach it without reaching the test process.
	interruptible bool
}

type configOptions struct {
	headerTimeout string         // upstream response-header timeout; default 5s
	maxBodyBytes  int64          // global request body ceiling; default 16384
	jwt           any            // trust configuration; default the test issuer
	audit         map[string]any // extra gateway audit settings
	admin         map[string]any // extra gateway admin session settings
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	e := &env{
		t:         t,
		dir:       dir,
		database:  filepath.Join(dir, "state.db"),
		auditLog:  filepath.Join(dir, "audit.log"),
		publicKey: filepath.Join(dir, "issuer.pem"),
		dataAddr:  freeAddr(t),
		admin:     freeAddr(t),
		metrics:   freeAddr(t),
		a:         newReceiver(t, "A"),
		b:         newReceiver(t, "B"),
		c:         newReceiver(t, "C"),
	}
	if err := os.WriteFile(e.publicKey, issuer.publicPEM(), 0o600); err != nil {
		t.Fatal(err)
	}
	e.writeConfig(configOptions{})
	t.Cleanup(e.stop)
	return e
}

// start launches the gateway and waits until both listeners accept and it
// has reported its protected admission.
func (e *env) start(binary string, extraEnv ...string) {
	e.t.Helper()
	e.run(binary, extraEnv...)
	e.waitReady()
}

// run launches the gateway's run command with e's configuration, without
// waiting for it to start.
func (e *env) run(binary string, extraEnv ...string) {
	e.t.Helper()
	e.launch(binary, []string{"run", "--config", e.config}, extraEnv...)
}

// waitOutput waits until the running gateway has written text.
func (e *env) waitOutput(text string) {
	e.t.Helper()
	waitFor(e.t, time.Now().Add(30*time.Second), fmt.Sprintf("%q in the gateway output", text), func() bool {
		return strings.Contains(e.output.String(), text)
	})
}

func (e *env) launch(binary string, args []string, extraEnv ...string) {
	e.t.Helper()
	e.stop()
	cmd := exec.Command(binary, args...)
	cmd.Env = append(os.Environ(),
		"GATEWAY_FAULT=", "GATEWAY_HOLD_PATH=", "GATEWAY_HOLD_FILE=", "GATEWAY_HOLD_RELEASE=", "GATEWAY_RATE_CLOCK=", "GATEWAY_WALL_OFFSET=", "GATEWAY_AUDIT_HOLD=",
	)
	if binaries.race {
		// Stop at the first race so it cannot be lost when the test later
		// kills the process; checkRaces reports it.
		cmd.Env = append(cmd.Env, "GORACE=halt_on_error=1")
	}
	cmd.Env = append(cmd.Env, extraEnv...)
	if e.interruptible {
		newProcessGroup(cmd)
	}
	e.output = &logBuffer{}
	cmd.Stdout, cmd.Stderr = e.output, e.output
	if err := cmd.Start(); err != nil {
		e.t.Fatal(err)
	}
	e.process = cmd
	e.exited = make(chan error, 1)
	go func() { e.exited <- cmd.Wait() }()
}

func (e *env) waitReady() {
	e.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-e.exited:
			e.process = nil
			e.t.Fatalf("gateway exited during startup: %v\n%s", err, e.output)
		default:
		}
		// Listening is not readiness: the gateway reports its protected
		// admission once it has started.
		observed := strings.Contains(e.output.String(), `"msg":"protected admission"`)
		if observed && dialable(e.dataAddr) && dialable(e.admin) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	e.t.Fatalf("gateway not ready\n%s", e.output)
}

func dialable(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// stop terminates the gateway abruptly. The design must tolerate this at any
// point, so tests never rely on a graceful shutdown.
func (e *env) stop() {
	e.sessionMu.Lock()
	e.session = adminSession{}
	e.sessionMu.Unlock()
	if e.process == nil {
		return
	}
	e.process.Process.Kill()
	<-e.exited
	e.process = nil
	e.checkRaces()
}

// checkRaces fails the test if the race detector reported anything in the
// gateway process. Call only after the process has exited, so the output is
// complete and no longer being written.
func (e *env) checkRaces() {
	e.t.Helper()
	if strings.Contains(e.output.String(), "WARNING: DATA RACE") {
		e.t.Errorf("race detected in gateway process:\n%s", e.output)
	}
}

// waitExit waits for a gateway that is expected to terminate itself.
func (e *env) waitExit() int {
	e.t.Helper()
	select {
	case <-e.exited:
		code := e.process.ProcessState.ExitCode()
		e.process = nil
		e.checkRaces()
		return code
	case <-time.After(30 * time.Second):
		e.t.Fatalf("gateway did not exit\n%s", e.output)
		return 0
	}
}
