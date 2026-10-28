package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	fixture "github.com/Alex-Zechariah-02/EAT40005-Gateway/internal/services/testfixture"
)

func main() {
	name := flag.String("service", "", "fixture to run: orders, inventory or reports")
	listen := flag.String("listen", "", "API address, a literal loopback address and port")
	control := flag.String("control", "", "control address, a literal loopback address and port")
	secretFile := flag.String("secret-file", "", "file holding the control secret")
	flag.Parse()

	if err := run(*name, *listen, *control, *secretFile); err != nil {
		fmt.Fprintln(os.Stderr, "fixture:", err)
		os.Exit(1)
	}
}

func run(name, listen, control, secretFile string) error {
	service, err := fixture.New(name)
	if err != nil {
		return err
	}
	secret, err := os.ReadFile(secretFile)
	if err != nil {
		return fmt.Errorf("reading control secret: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return fixture.Run(ctx, service, listen, control, []byte(strings.TrimSpace(string(secret))))
}
