package gateway

import (
	"os/exec"
	"syscall"
	"testing"
)

// newProcessGroup starts the command as the root of a new console process
// group, so that a console break event can be sent to it and its group only.
func newProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// interrupt sends Ctrl+Break to the process group of cmd, which Go delivers
// to a program that waits for os.Interrupt. Process.Signal cannot send
// os.Interrupt on Windows, and Ctrl+C cannot be sent to a single group.
func interrupt(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	generate := syscall.NewLazyDLL("kernel32.dll").NewProc("GenerateConsoleCtrlEvent")
	if ok, _, err := generate.Call(syscall.CTRL_BREAK_EVENT, uintptr(cmd.Process.Pid)); ok == 0 {
		t.Fatalf("sending Ctrl+Break to process %d: %v", cmd.Process.Pid, err)
	}
}
