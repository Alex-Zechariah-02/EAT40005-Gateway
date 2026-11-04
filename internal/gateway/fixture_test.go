// Fixture process helpers and reproducible reset and replay.

package gateway

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	fixture "github.com/Alex-Zechariah-02/EAT40005-Gateway/internal/services/testfixture"
)

var fixtureNames = []string{"orders", "inventory", "reports"}

// fixtureProcess is one fixture API running as its own process.
type fixtureProcess struct {
	t       *testing.T
	name    string
	addr    string // API listener, reached only through the gateway
	control string // receipts, state and reset; requires the secret
	secret  string
	output  *bytes.Buffer
}

// startFixtures runs the three fixture APIs on loopback, each with its own
// control secret, and stops them when the test ends.
func startFixtures(t *testing.T) map[string]*fixtureProcess {
	t.Helper()
	return startFixtureSet(t, binaries.fixture)
}

func startFixtureSet(t *testing.T, binary string, extraEnv ...string) map[string]*fixtureProcess {
	t.Helper()
	dir := t.TempDir()
	fixtures := map[string]*fixtureProcess{}
	for _, name := range fixtureNames {
		secret := make([]byte, 32)
		rand.Read(secret)
		f := &fixtureProcess{
			t:       t,
			name:    name,
			addr:    freeAddr(t),
			control: freeAddr(t),
			secret:  hex.EncodeToString(secret),
			output:  &bytes.Buffer{},
		}
		secretFile := filepath.Join(dir, name+".secret")
		if err := os.WriteFile(secretFile, []byte(f.secret), 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(binary, "-service", name, "-listen", f.addr, "-control", f.control, "-secret-file", secretFile)
		cmd.Env = append(os.Environ(), "FIXTURE_FAULT=")
		cmd.Env = append(cmd.Env, extraEnv...)
		if binaries.race {
			cmd.Env = append(cmd.Env, "GORACE=halt_on_error=1")
		}
		newProcessGroup(cmd)
		cmd.Stdout, cmd.Stderr = f.output, f.output
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		exited := make(chan error, 1)
		go func() { exited <- cmd.Wait() }()
		t.Cleanup(func() {
			interrupt(t, cmd)
			select {
			case err := <-exited:
				if err != nil {
					t.Errorf("fixture %s did not stop cleanly: %v\n%s", name, err, f.output)
				}
			case <-time.After(6 * time.Second):
				cmd.Process.Kill()
				<-exited
				t.Errorf("fixture %s shutdown exceeded its bound", name)
			}
			if strings.Contains(f.output.String(), "WARNING: DATA RACE") {
				t.Errorf("race detected in fixture %s:\n%s", name, f.output)
			}
		})
		deadline := time.Now().Add(10 * time.Second)
		for !dialable(f.addr) || !dialable(f.control) {
			if time.Now().After(deadline) {
				t.Fatalf("fixture %s not ready\n%s", name, f.output)
			}
			time.Sleep(20 * time.Millisecond)
		}
		fixtures[name] = f
	}
	return fixtures
}

func (f *fixtureProcess) call(method, path, secret string) (int, []byte) {
	f.t.Helper()
	req, _ := http.NewRequest(method, "http://"+f.control+path, http.NoBody)
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		f.t.Fatalf("fixture %s control %s: %v", f.name, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

func (f *fixtureProcess) receipts() []fixture.Receipt {
	f.t.Helper()
	code, data := f.call(http.MethodGet, "/receipts", f.secret)
	var body struct{ Receipts []fixture.Receipt }
	if code != http.StatusOK || json.Unmarshal(data, &body) != nil {
		f.t.Fatalf("fixture %s receipts: %d %s", f.name, code, data)
	}
	return body.Receipts
}

func (f *fixtureProcess) state() fixture.State {
	f.t.Helper()
	return f.decodeState(f.call(http.MethodGet, "/state", f.secret))
}

func (f *fixtureProcess) reset() fixture.State {
	f.t.Helper()
	return f.decodeState(f.call(http.MethodPost, "/reset", f.secret))
}

func (f *fixtureProcess) decodeState(code int, data []byte) fixture.State {
	f.t.Helper()
	var s fixture.State
	if code != http.StatusOK || json.Unmarshal(data, &s) != nil {
		f.t.Fatalf("fixture %s state: %d %s", f.name, code, data)
	}
	return s
}

func receiptCounts(fixtures map[string]*fixtureProcess) map[string]int {
	counts := map[string]int{}
	for name, f := range fixtures {
		counts[name] = len(f.receipts())
	}
	return counts
}

// newFixtureEnv starts the three fixtures and a gateway serving fixturePolicy.
func newFixtureEnv(t *testing.T) (*env, map[string]*fixtureProcess) {
	t.Helper()
	fixtures := startFixtures(t)
	e := newEnv(t)
	seed(t, e.database, fixturePolicy(fixtures), seedOptions{})
	e.start(binaries.plain)
	if code, body, _ := e.harnessCall("PUT", "/admin/blocked-accounts/former-01", `{"reason":"disabled test account"}`, "Idempotency-Key", "fixture-disabled-account"); code != http.StatusOK {
		t.Fatalf("seed containment: %d %s", code, body)
	}
	return e, fixtures
}

// Reset restores the seed exactly, and replaying the
// catalogue from it reproduces the same outcomes and final state.
func TestResetAndReplayAreReproducible(t *testing.T) {
	e, fixtures := newFixtureEnv(t)
	c := loadCatalogue(t)
	seedHash := map[string]string{}
	for _, f := range c.Fixtures {
		seedHash[f.Service] = f.SeedSHA256
	}

	type run struct {
		results []stepResult
		final   map[string]string
	}
	var runs []run
	for range 2 {
		for name, f := range fixtures {
			s := f.reset()
			if s.SHA256 != seedHash[name] || len(f.receipts()) != 0 {
				t.Fatalf("reset of %s: state %s with %d receipts, want seed %s and none", name, s.SHA256, len(f.receipts()), seedHash[name])
			}
		}
		r := run{results: runScenarios(t, e, fixtures, c), final: map[string]string{}}
		for name, f := range fixtures {
			r.final[name] = f.state().SHA256
		}
		runs = append(runs, r)
	}

	if !slices.Equal(runs[0].results, runs[1].results) {
		t.Fatalf("replays differ:\n%v\n%v", runs[0].results, runs[1].results)
	}
	for name := range fixtures {
		if runs[0].final[name] != runs[1].final[name] {
			t.Errorf("fixture %s final state differs between replays", name)
		}
	}
	// The catalogue changes orders and inventory, and never reports.
	if runs[0].final["orders"] == seedHash["orders"] || runs[0].final["inventory"] == seedHash["inventory"] || runs[0].final["reports"] != seedHash["reports"] {
		t.Fatalf("final states %v against seeds %v", runs[0].final, seedHash)
	}
	t.Logf("final states after each replay: %v", runs[0].final)
}
