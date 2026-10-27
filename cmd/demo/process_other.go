//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"

	"golang.org/x/sys/unix"
)

// readSecretLine reads one line from the terminal without showing it. Input
// that is not a terminal, such as a pipe, is read as it is: nothing shows it.
func readSecretLine(prompt string) (string, error) {
	fmt.Print(prompt)
	if stty("-echo") != nil {
		defer fmt.Println()
		return readLine()
	}
	defer func() {
		stty("echo")
		fmt.Println()
	}()
	return readLine()
}

func stty(setting string) error {
	command := exec.Command("stty", setting)
	command.Stdin = os.Stdin
	return command.Run()
}

func newProcessGroup(*exec.Cmd) {}

func interruptProcess(cmd *exec.Cmd) error {
	return cmd.Process.Signal(os.Interrupt)
}

func lockEarlierDemoFile(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func privateDirectory(path string) error {
	return os.Mkdir(path, 0o700)
}
