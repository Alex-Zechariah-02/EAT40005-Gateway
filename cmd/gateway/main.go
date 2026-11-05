// Command gateway runs the security policy gateway on its own Go HTTP server,
// from one strict native configuration file, and its local operator commands.
package main

import (
	"os"

	"github.com/Alex-Zechariah-02/apiward/internal/gateway"
	"github.com/Alex-Zechariah-02/apiward/internal/openapi"
)

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "policy" {
		os.Exit(openapi.Command(args[1:], os.Stderr))
	}
	os.Exit(gateway.Main(args))
}
