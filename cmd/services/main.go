package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/Alex-Zechariah-02/apiward/internal/services"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := services.Command(ctx, "", os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "services:", err)
		os.Exit(1)
	}
}
