package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// readSecretLine reads one line from the console without showing it. Input
// that is not a console, such as a pipe, is read as it is: nothing shows it.
func readSecretLine(prompt string) (string, error) {
	fmt.Print(prompt)
	console := windows.Handle(os.Stdin.Fd())
	var mode uint32
	if windows.GetConsoleMode(console, &mode) != nil {
		defer fmt.Println()
		return readLine()
	}
	if err := windows.SetConsoleMode(console, mode&^windows.ENABLE_ECHO_INPUT); err != nil {
		return "", err
	}
	defer func() {
		windows.SetConsoleMode(console, mode)
		fmt.Println()
	}()
	return readLine()
}

func newProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

func interruptProcess(cmd *exec.Cmd) error {
	return windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(cmd.Process.Pid))
}

func lockEarlierDemoFile(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	if err := windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{}); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

// The new directory receives its DACL atomically, before any signing key is
// written. Child files inherit current-user, SYSTEM and administrator access;
// broad permissions on a demonstration's parent are not inherited.
func privateDirectory(path string) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;" + user.User.Sid.String() + ")(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)")
	if err != nil {
		return err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	attributes := windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: descriptor,
	}
	if err := windows.CreateDirectory(name, &attributes); err != nil {
		return &os.PathError{Op: "mkdir", Path: path, Err: err}
	}
	return nil
}
