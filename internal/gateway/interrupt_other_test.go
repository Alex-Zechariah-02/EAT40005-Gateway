//go:build !windows

package gateway

import (
	"os"
	"os/exec"
	"testing"
)

// newProcessGroup needs nothing outside Windows: a signal reaches one
// process.
func newProcessGroup(*exec.Cmd) {}

// interrupt sends SIGINT to the process of cmd.
func interrupt(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("interrupting process %d: %v", cmd.Process.Pid, err)
	}
}
