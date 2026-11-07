// Command gateway runs the security policy gateway on its own Go HTTP server,
// from one strict native configuration file, and its local operator commands.
package main

import (
	"os"

	"github.com/Alex-Zechariah-02/apiward/internal/gateway"
	"github.com/Alex-Zechariah-02/apiward/internal/migrate"
	"github.com/Alex-Zechariah-02/apiward/internal/openapi"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

// run sends policy and migration subcommands to their tools and everything
// else, including "migrate --config", to the gateway.
func run(args []string) int {
	switch {
	case len(args) > 0 && args[0] == "policy":
		return openapi.Command(args[1:], os.Stderr)
	case len(args) > 1 && args[0] == "migrate" && migrate.Handles(args[1]):
		return migrate.Command(args[1:], os.Stderr)
	}
	return gateway.Main(args)
}
