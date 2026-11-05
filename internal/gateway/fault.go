//go:build faultinject

package gateway

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"
)

// Fault injection and test controls exist only in binaries built with the
// faultinject tag. The point to trigger is chosen by the GATEWAY_FAULT
// environment variable.

const faultExitCode = 86

func faultEnabled(name string) bool { return os.Getenv("GATEWAY_FAULT") == name }

func candidateReadFault() error {
	if faultEnabled("candidate-read-unavailable") {
		return errors.New("injected candidate read failure")
	}
	return nil
}

// cancelPoint waits at the named point until the caller's context is done or
// two seconds pass, so a test can disconnect a client exactly there.
func cancelPoint(ctx context.Context, name string) {
	if !faultEnabled(name) {
		return
	}
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
	}
}

// crashPoint terminates the process abruptly, without cleanup, to simulate a
// crash at a precise point in a transition.
func crashPoint(name string) {
	if faultEnabled(name) {
		os.Exit(faultExitCode)
	}
}

// writeAudit appends data and syncs it, unless a fault is injected: no space
// for any write, a failed sync after any write, and, for a batch holding an
// admission, a write of half the data, a crash between write and sync, or a
// sync delayed past the acknowledgement wait; or a failed write of a batch
// holding a request outcome or the outcome of a publishing transition. The
// writer may also be held before it writes; see holdAudit.
func writeAudit(file *os.File, data []byte) error {
	holdAudit()
	admission := bytes.Contains(data, []byte(`"kind":"request.admitted"`))
	outcome := bytes.Contains(data, []byte(`"kind":"request.forwarded"`))
	published := false
	for _, kind := range []string{"transition.published", "cpa.activated", "cpa.confirmed", "cpa.rolled_back", "cpa.timed_out",
		"tpo.created", "tpo.revoked", "tpo.expired", "account.block", "account.unblock", "keys.reload"} {
		published = published || bytes.Contains(data, []byte(`"kind":"`+kind+`"`))
	}
	switch {
	case faultEnabled("audit-full"):
		return errors.New("injected audit failure: no space left on device")
	case faultEnabled("audit-fail-outcome") && outcome:
		return errors.New("injected audit failure writing an outcome")
	case faultEnabled("audit-fail-published") && published:
		return errors.New("injected audit failure writing a transition outcome")
	case faultEnabled("audit-partial") && admission:
		file.Write(data[:len(data)/2])
		return errors.New("injected audit failure after a partial write")
	}
	if _, err := file.Write(data); err != nil {
		return err
	}
	switch {
	case faultEnabled("audit-sync"):
		return errors.New("injected audit sync failure")
	case faultEnabled("audit-crash-before-sync") && admission:
		os.Exit(faultExitCode)
	case faultEnabled("audit-slow-sync") && admission:
		time.Sleep(3 * time.Second)
	}
	return file.Sync()
}

// holdAudit holds the writer before a write while the file GATEWAY_AUDIT_HOLD
// names exists, so a test can queue events behind a writer held at a known
// point and decide when it continues. Once held, the writer creates the
// file's ".held" companion.
func holdAudit() {
	hold := os.Getenv("GATEWAY_AUDIT_HOLD")
	if hold == "" {
		return
	}
	if _, err := os.Stat(hold); err != nil {
		return
	}
	os.WriteFile(hold+".held", nil, 0o600)
	for {
		if _, err := os.Stat(hold); err != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// rateClockFault lets tests set the limiter's clock: when GATEWAY_RATE_CLOCK
// names a file holding Unix milliseconds, the limiter reads its time there.
func rateClockFault() (int64, bool) {
	path := os.Getenv("GATEWAY_RATE_CLOCK")
	if path == "" {
		return 0, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	ms, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	return ms, err == nil
}

// wallOffset lets tests move the controller's wall clock without changing the
// host clock: when GATEWAY_WALL_OFFSET names a file holding a number of
// milliseconds, the wall clock reads that much later (or earlier, if
// negative). The monotonic clock is not affected.
func wallOffset() time.Duration {
	path := os.Getenv("GATEWAY_WALL_OFFSET")
	if path == "" {
		return 0
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	ms, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0
	}
	return time.Duration(ms) * time.Millisecond
}

// timerFault suppresses the deadline timer so tests can observe the request
// path after a deadline passes but before the controller acts on it.
func timerFault() bool { return faultEnabled("no-deadline-timer") }

// rollbackFault makes the base that a rollback restores fail to compile, as a
// damaged rollback target would.
func rollbackFault() error {
	if faultEnabled("rollback-target-invalid") {
		return errors.New("injected failure compiling the rollback target")
	}
	return nil
}

// signalCapture appends the captured generation to a file when a request for
// the configured path has captured its generation, so a test can act at that
// exact point instead of sleeping. When GATEWAY_HOLD_RELEASE names a file, the
// request then waits, for at most ten seconds, until that file exists, so a
// test can publish a new generation before any later stage runs.
func signalCapture(path, generation string) {
	if path == "" || path != os.Getenv("GATEWAY_HOLD_PATH") {
		return
	}
	file, err := os.OpenFile(os.Getenv("GATEWAY_HOLD_FILE"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	file.WriteString(generation + "\n")
	file.Close()

	release := os.Getenv("GATEWAY_HOLD_RELEASE")
	if release == "" {
		return
	}
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		if _, err := os.Stat(release); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}
