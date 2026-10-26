//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
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

func privateDirectory(path string) error {
	return os.Mkdir(path, 0o700)
}
